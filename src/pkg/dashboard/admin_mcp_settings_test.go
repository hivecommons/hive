package dashboard

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
)

// settingsReadExcludedRoutes are the GET /api/config routes settings_read does
// not reach, each with the reason. Every other GET /api/config route must be a
// settings_read section, so whatever the Settings page can show, the admin MCP
// can read (TestSettingsReadCoversEveryConfigRoute).
var settingsReadExcludedRoutes = map[string]string{
	"/api/config/download":            "the raw hive.yaml, credentials included: the exclusion of anything that reads back a credential",
	"/api/config/export":              "the whole configuration export with its layers and side files; config_effective_read is the admin MCP's vetted view of the effective configuration",
	"/api/config/export.json":         "the same export as /api/config/export",
	"/api/config/provenance":          "config_effective_read returns the provenance (layers, overlay, last-good, ratchet)",
	"/api/config/agent/{name}/export": "a download bundle of the agent and agent_prompt sections",
	"/api/config/dashboard/theme":     "purely presentational (the exclusion of presentational endpoints)",
	"/api/config/sidebar":             "purely presentational (the exclusion of presentational endpoints)",
}

// settingsPageOtherRoutes are the GET routes outside /api/config that Settings
// tabs load when opened (renderConfigTab in static/index.html). Each must be a
// settings_read section too.
var settingsPageOtherRoutes = []string{
	"/api/upstream-watch",
	"/api/knowledge/public",
	"/api/linear/agent/status",
	"/api/compliance/status",
	"/api/classifier/stats",
	"/api/watchdog/activity",
}

var getRouteRE = regexp.MustCompile(`HandleFunc\("GET (/api/[^"]*)"`)

// registeredGetRoutes lists every GET /api route this package registers.
func registeredGetRoutes(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range getRouteRE.FindAllStringSubmatch(string(src), -1) {
			seen[m[1]] = true
		}
	}
	routes := make([]string, 0, len(seen))
	for r := range seen {
		routes = append(routes, r)
	}
	sort.Strings(routes)
	if len(routes) < 100 {
		t.Fatalf("found only %d GET /api routes; the route scan is broken", len(routes))
	}
	return routes
}

// Every GET /api/config route the dashboard serves is a settings_read section
// or a listed exclusion, and every section names a served route. A new
// Settings endpoint fails here until it is given a section or a reason.
func TestSettingsReadCoversEveryConfigRoute(t *testing.T) {
	sections := map[string]string{}
	for _, s := range adminmcp.SettingsSections() {
		sections[strings.Replace(s.Path, "{agent}", "{name}", 1)] = s.Name
	}
	served := map[string]bool{}
	for _, r := range registeredGetRoutes(t) {
		served[r] = true
		if !strings.HasPrefix(r, "/api/config") {
			continue
		}
		_, isSection := sections[r]
		_, excluded := settingsReadExcludedRoutes[r]
		switch {
		case isSection && excluded:
			t.Errorf("%s is both a settings_read section and excluded", r)
		case !isSection && !excluded:
			t.Errorf("GET %s is not reachable through settings_read: add a section in pkg/adminmcp/tools_settings.go, or an exclusion with its reason", r)
		}
	}
	for _, r := range settingsPageOtherRoutes {
		if _, ok := sections[r]; !ok {
			t.Errorf("the Settings page loads GET %s, which settings_read does not reach", r)
		}
	}
	for path, name := range sections {
		if !served[path] {
			t.Errorf("settings_read section %q names %s, which the dashboard does not serve", name, path)
		}
	}
	for path := range settingsReadExcludedRoutes {
		if !served[path] {
			t.Errorf("excluded route %s is not served; drop it from settingsReadExcludedRoutes", path)
		}
	}
}

// settingsFill sets every exported field of a config to a distinct value:
// strings get a sentinel, slices and maps one entry, booleans true, numbers
// non-zero. It records which config path each sentinel came from.
type settingsFill struct {
	n     int
	paths map[string]string
}

func (f *settingsFill) fill(v reflect.Value, path string, depth int) {
	if depth > 12 || !v.CanSet() {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		f.fill(v.Elem(), path, depth+1)
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			if !sf.IsExported() {
				continue
			}
			name := strings.Split(sf.Tag.Get("yaml"), ",")[0]
			if name == "-" {
				continue
			}
			p := path
			if name == "" && !sf.Anonymous {
				name = strings.ToLower(sf.Name)
			}
			if name != "" {
				if p != "" {
					p += "."
				}
				p += name
			}
			f.fill(v.Field(i), p, depth+1)
		}
	case reflect.String:
		f.n++
		s := fmt.Sprintf("Snt%04dq", f.n)
		f.paths[s] = path
		v.SetString(s)
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return
		}
		sl := reflect.MakeSlice(v.Type(), 1, 1)
		f.fill(sl.Index(0), path+"[0]", depth+1)
		v.Set(sl)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		if k.Kind() == reflect.String {
			f.n++
			key := fmt.Sprintf("Key%04dq", f.n)
			f.paths[key] = path + ".<key>"
			k.SetString(key)
		}
		e := reflect.New(v.Type().Elem()).Elem()
		f.fill(e, path+".*", depth+1)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64, reflect.Int32:
		v.SetInt(7)
	case reflect.Float64, reflect.Float32:
		v.SetFloat(0.5)
	}
}

var settingsSentinelRE = regexp.MustCompile(`(?:Snt|Key)\d{4}q`)

// A config field that holds a secret value itself, not the name of the
// environment variable, file or header that supplies one.
var (
	settingsSecretLeafRE = regexp.MustCompile(`(?i)(token|secret|password|passphrase|authorization|api[_-]?key|apikey|private[_-]?key|webhook)`)
	settingsSecretRefRE  = regexp.MustCompile(`(?i)(_env|_file|_header|_name|_var|_label|_id)$`)
)

func settingsSecretPath(path string) bool {
	if strings.Contains(path, ".env.") || strings.Contains(path, ".headers.") {
		return true
	}
	leaf := path
	if i := strings.LastIndex(leaf, "."); i >= 0 {
		leaf = leaf[i+1:]
	}
	leaf = strings.TrimSuffix(leaf, "[0]")
	return settingsSecretLeafRE.MatchString(leaf) && !settingsSecretRefRE.MatchString(leaf)
}

// Every settings_read section answers a verified owner through the admin MCP,
// with every config field set, and no answer carries a value from a field
// that holds a secret: the endpoints name where a secret comes from, and
// Scrub masks the rest.
func TestSettingsReadSectionsAnswerWithoutSecrets(t *testing.T) {
	deps := testDeps(t)
	f := &settingsFill{paths: map[string]string{}}
	f.fill(reflect.ValueOf(deps.Config).Elem(), "", 0)
	agent := ""
	for name := range deps.Config.Agents {
		agent = name
	}
	s := NewServer(0, deps.Logger)
	s.RegisterAPI(deps)
	owner := dashboardAdminMCPProvider{server: s, role: config.RoleOwner, ownerVerified: true}
	secretsSeen := 0
	for _, sec := range adminmcp.SettingsSections() {
		data, err := owner.Read(context.Background(), adminmcp.ToolSettingsRead, map[string]any{"section": sec.Name, "agent": agent})
		if err != nil {
			t.Errorf("section %s: %v", sec.Name, err)
			continue
		}
		b, err := adminmcp.EncodeResult(data)
		if err != nil {
			t.Fatalf("section %s: encode: %v", sec.Name, err)
		}
		for _, m := range settingsSentinelRE.FindAllString(string(b), -1) {
			if path := f.paths[m]; settingsSecretPath(path) {
				secretsSeen++
				t.Errorf("section %s returns the value of %s", sec.Name, path)
			}
		}
	}
	// The fill must reach the secret fields, or the check above proves
	// nothing.
	secretFields := 0
	for _, path := range f.paths {
		if settingsSecretPath(path) {
			secretFields++
		}
	}
	if secretFields < 10 {
		t.Fatalf("only %d secret fields filled; the config fill is broken", secretFields)
	}
}

func TestSettingsReadSectionArguments(t *testing.T) {
	s, _ := apiServer(t)
	owner := dashboardAdminMCPProvider{server: s, role: config.RoleOwner, ownerVerified: true}
	ctx := context.Background()
	gov, err := owner.Read(ctx, adminmcp.ToolSettingsRead, nil)
	if err != nil {
		t.Fatal(err)
	}
	named, err := owner.Read(ctx, adminmcp.ToolSettingsRead, map[string]any{"section": "governor"})
	if err != nil || !reflect.DeepEqual(gov, named) {
		t.Fatalf("no section must read governor: %v", err)
	}
	am, err := owner.Read(ctx, adminmcp.ToolSettingsRead, map[string]any{"section": "auto_merge"})
	if m, _ := am.(map[string]any); err != nil || m == nil || reflect.DeepEqual(am, gov) {
		t.Fatalf("auto_merge section = %#v, %v", am, err)
	}
	if _, err := owner.Read(ctx, adminmcp.ToolSettingsRead, map[string]any{"section": "../governor"}); err == nil || !strings.Contains(err.Error(), "unknown settings_read section") {
		t.Fatalf("unknown section: %v", err)
	}
	if _, err := owner.Read(ctx, adminmcp.ToolSettingsRead, map[string]any{"section": "agent"}); err == nil || !strings.Contains(err.Error(), "needs agent") {
		t.Fatalf("agent section without agent: %v", err)
	}
	ag, err := owner.Read(ctx, adminmcp.ToolSettingsRead, map[string]any{"section": "agent", "agent": "scanner"})
	if err != nil || !strings.Contains(fmt.Sprint(ag), "sonnet") {
		t.Fatalf("agent section = %#v, %v", ag, err)
	}

	// An owner role this hive did not verify is not promoted: an owner-only
	// Settings endpoint refuses it, as it refuses the same caller in the
	// dashboard, while the governor section keeps answering.
	claimed := dashboardAdminMCPProvider{server: s, role: config.RoleOwner}
	if _, err := claimed.Read(ctx, adminmcp.ToolSettingsRead, map[string]any{"section": "auto_merge"}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("auto_merge for a claimed owner: %v", err)
	}
	if _, err := claimed.Read(ctx, adminmcp.ToolSettingsRead, nil); err != nil {
		t.Fatalf("governor for a claimed owner: %v", err)
	}
	reader := dashboardAdminMCPProvider{server: s, role: config.RoleRead, ownerVerified: true}
	if _, err := reader.Read(ctx, adminmcp.ToolSettingsRead, map[string]any{"section": "auto_merge"}); err == nil {
		t.Fatal("a read role must not read an owner-only section")
	}
}
