package compliance

import (
	"errors"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/hivecommons/hive/pkg/config"
)

func mustProfile(t *testing.T, id string) Profile {
	t.Helper()
	p, ok := ProfileByID(id)
	if !ok {
		t.Fatalf("profile %s not found", id)
	}
	return p
}

func TestEmbeddedProfilesLoad(t *testing.T) {
	ps, err := Profiles()
	if err != nil {
		t.Fatalf("Profiles: %v", err)
	}
	if len(ps) == 0 {
		t.Fatal("no embedded profiles")
	}
	if _, ok := ProfileByID(" SOC2-Type2 "); !ok {
		t.Fatal("ProfileByID must be case/space-insensitive")
	}
	if _, ok := ProfileByID("nope"); ok {
		t.Fatal("unknown id must not resolve")
	}
	// Profiles returns a copy: mutating it must not leak into the cache.
	ps[0].ID = "mutated"
	again, _ := Profiles()
	if again[0].ID == "mutated" {
		t.Fatal("Profiles must return a defensive copy")
	}
}

// TestProfileSchema is the acceptance schema check for every shipped
// profile: identifying fields present, domains known, every mapping naming a
// known setting path and evaluator, and covered/not-covered exclusive.
func TestProfileSchema(t *testing.T) {
	ps, err := Profiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if err := p.Validate(); err != nil {
			t.Errorf("%s: %v", p.ID, err)
		}
		for _, c := range p.Controls {
			if c.ID == "" || c.Title == "" || c.Citation == "" {
				t.Errorf("%s: control %+v missing id/title/citation", p.ID, c)
			}
			if c.NotCovered && c.Rationale == "" {
				t.Errorf("%s %s: not_covered without rationale", p.ID, c.ID)
			}
			for _, m := range c.Mappings {
				if _, ok := settings[m.SettingPath]; !ok {
					t.Errorf("%s %s: unknown setting path %q", p.ID, c.ID, m.SettingPath)
				}
				if _, ok := evaluators[m.Evaluator]; !ok {
					t.Errorf("%s %s: unknown evaluator %q", p.ID, c.ID, m.Evaluator)
				}
			}
		}
	}
}

func TestSOC2ProfileCoversFullTrustServicesCriteria(t *testing.T) {
	p := mustProfile(t, "soc2-type2")
	wantIDs := []string{
		"CC1.1", "CC1.2", "CC1.3", "CC1.4", "CC1.5",
		"CC2.1", "CC2.2", "CC2.3",
		"CC3.1", "CC3.2", "CC3.3", "CC3.4",
		"CC4.1", "CC4.2",
		"CC5.1", "CC5.2", "CC5.3",
		"CC6.1", "CC6.2", "CC6.3", "CC6.4", "CC6.5", "CC6.6", "CC6.7", "CC6.8",
		"CC7.1", "CC7.2", "CC7.3", "CC7.4", "CC7.5",
		"CC8.1",
		"CC9.1", "CC9.2",
		"A1.1", "A1.2", "A1.3",
		"C1.1", "C1.2",
		"PI1.1", "PI1.2", "PI1.3", "PI1.4", "PI1.5",
		"P1.1", "P2.1", "P3.1", "P3.2", "P4.1", "P4.2", "P4.3",
		"P5.1", "P5.2", "P6.1", "P6.2", "P6.3", "P6.4", "P6.5", "P6.6", "P6.7",
		"P7.1", "P8.1",
	}
	gotIDs := make([]string, 0, len(p.Controls))
	paths := map[string]bool{}
	notCovered := 0
	for _, c := range p.Controls {
		gotIDs = append(gotIDs, c.ID)
		if c.NotCovered {
			notCovered++
		}
		for _, m := range c.Mappings {
			paths[m.SettingPath] = true
		}
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Errorf("soc2-type2 control IDs = %v, want complete TSC list %v", gotIDs, wantIDs)
	}
	for _, want := range []string{
		"dashboard.authorized_users", "review.require_approval", "auto_merge.human_merge_paths",
		"auto_merge.trusted_authors.enabled", "sentinel.enabled", "agent_sandbox.enabled",
		"audit.retention_days", "env." + config.ProxyInjectGHAuthEnv, "acmm_level",
		"governor.backup.key_file",
	} {
		if !paths[want] {
			t.Errorf("soc2-type2 maps nothing to %s", want)
		}
	}
	if notCovered == 0 {
		t.Error("soc2-type2 must mark genuinely uncovered controls not_covered")
	}
}

// TestShippedProfilesCoverTheirScope pins what each profile is for: FedRAMP
// stays inside the families the epic lists (plus explicitly not-covered
// out-of-subset markers), and every profile marks what Hive cannot evidence
// and maps real settings for the rest.
func TestShippedProfilesCoverTheirScope(t *testing.T) {
	families := map[string]bool{"AC": true, "AU": true, "CM": true, "IA": true, "RA": true, "SA": true, "SI": true}
	for _, id := range []string{"soc2-type2", "fedramp-moderate", "iso27001-annex-a"} {
		p := mustProfile(t, id)
		covered, notCovered := 0, 0
		for _, c := range p.Controls {
			if c.NotCovered {
				notCovered++
			} else {
				covered++
			}
			if id == "fedramp-moderate" && !c.NotCovered {
				if fam := strings.SplitN(c.ID, "-", 2)[0]; !families[fam] {
					t.Errorf("fedramp-moderate covered control %s is outside the AC/AU/CM/IA/RA/SA/SI subset", c.ID)
				}
			}
		}
		if covered < 10 || notCovered == 0 {
			t.Errorf("%s: covered=%d not_covered=%d", id, covered, notCovered)
		}
	}
	for _, tc := range []struct {
		profile string
		ids     []string
	}{
		{"fedramp-moderate", []string{"AC-17", "AU-8", "AU-9", "CM-2", "CM-7", "CM-8", "IA-8", "IR-4", "IR-6", "SC-7", "SC-8", "SC-28", "SI-2", "SI-3", "CP-9", "SR-2", "ELIG-1"}},
		{"iso27001-annex-a", []string{"A.5.23", "A.5.28", "A.8.3", "A.8.5", "A.8.12", "A.8.13", "A.8.20", "A.8.21", "A.8.22", "A.8.23", "A.8.24", "A.8.26", "A.8.30"}},
	} {
		pp := mustProfile(t, tc.profile)
		got := map[string]bool{}
		for _, c := range pp.Controls {
			got[c.ID] = c.NotCovered
		}
		if _, ok := got["AC-2.ELIG"]; ok {
			t.Errorf("%s: AC-2.ELIG is not a NIST identifier", tc.profile)
		}
		for _, id := range tc.ids {
			if nc, ok := got[id]; !ok || !nc {
				t.Errorf("%s: %s must be present and not_covered", tc.profile, id)
			}
		}
	}
	fed := mustProfile(t, "fedramp-moderate")
	have := map[string]bool{}
	for _, c := range fed.Controls {
		have[c.ID] = true
	}
	for _, want := range []string{"AC-2", "AC-3", "AC-5", "AC-6", "AU-2", "AU-3", "AU-6", "AU-11", "AU-12", "CM-3", "CM-5", "CM-6", "IA-2", "RA-5", "SA-11", "SI-4", "SI-7", "PE-3", "PS-3"} {
		if !have[want] {
			t.Errorf("fedramp-moderate missing %s", want)
		}
	}
	iso := mustProfile(t, "iso27001-annex-a")
	have = map[string]bool{}
	for _, c := range iso.Controls {
		have[c.ID] = true
	}
	for _, want := range []string{"A.5.1", "A.5.15", "A.8.9", "A.8.15", "A.8.25", "A.8.32", "A.6.1", "A.7.2"} {
		if !have[want] {
			t.Errorf("iso27001-annex-a missing %s", want)
		}
	}
}

// TestKnownFrameworksMatchConfig keeps config.KnownComplianceFrameworks (which
// config validation uses, and which cannot import this package) equal to the
// embedded profile set.
func TestKnownFrameworksMatchConfig(t *testing.T) {
	ps, err := Profiles()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range ps {
		ids = append(ids, p.ID)
	}
	want := append([]string(nil), config.KnownComplianceFrameworks...)
	sort.Strings(want)
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("embedded profiles %v != config.KnownComplianceFrameworks %v", ids, want)
	}
}

// TestSettingPathsExistOnConfig walks config.Config's YAML tags so a setting
// path can never name a key the config does not have. env.* and builtin
// settings are the two documented exceptions.
func TestSettingPathsExistOnConfig(t *testing.T) {
	for path, s := range settings {
		if s.Builtin {
			continue
		}
		if strings.HasPrefix(path, "env.") {
			if strings.TrimPrefix(path, "env.") == "" {
				t.Errorf("%s: empty env var name", path)
			}
			continue
		}
		if !yamlPathExists(reflect.TypeOf(config.Config{}), strings.Split(path, ".")) {
			t.Errorf("setting path %q is not a YAML key of config.Config", path)
		}
	}
}

func yamlPathExists(typ reflect.Type, parts []string) bool {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if len(parts) == 0 {
		return true
	}
	if typ.Kind() != reflect.Struct {
		return false
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if f.Anonymous && name == "" && strings.Contains(opts, "inline") {
			if yamlPathExists(f.Type, parts) {
				return true
			}
			continue
		}
		if name == parts[0] {
			return yamlPathExists(f.Type, parts[1:])
		}
	}
	return false
}

func TestYAMLPathExistsRejectsUnknown(t *testing.T) {
	typ := reflect.TypeOf(config.Config{})
	if yamlPathExists(typ, []string{"review", "nope"}) {
		t.Fatal("unknown leaf must not exist")
	}
	if yamlPathExists(typ, []string{"hive_id", "deeper"}) {
		t.Fatal("cannot descend into a scalar")
	}
}

func TestKnownSettingsAndEvaluators(t *testing.T) {
	ks := KnownSettings()
	if len(ks) != len(settings) {
		t.Fatalf("KnownSettings len %d != %d", len(ks), len(settings))
	}
	if !sort.SliceIsSorted(ks, func(i, j int) bool { return ks[i].Path < ks[j].Path }) {
		t.Fatal("KnownSettings must be sorted")
	}
	builtin := 0
	for _, s := range ks {
		if s.Description == "" || s.Kind == "" {
			t.Errorf("%s: missing kind/description", s.Path)
		}
		if s.Builtin {
			builtin++
		}
	}
	if builtin != 1 {
		t.Fatalf("builtin settings = %d, want 1 (audit.retention_days)", builtin)
	}
	ev := Evaluators()
	if len(ev) != len(evaluators) || !sort.StringsAreSorted(ev) {
		t.Fatalf("Evaluators() = %v must list every evaluator, sorted", ev)
	}
	for _, name := range ev {
		if _, ok := evaluators[name]; !ok {
			t.Errorf("Evaluators() lists unknown %q", name)
		}
	}
}

func validProfileYAML(id string) string {
	return "id: " + id + "\nname: Test\nversion: \"1\"\ncontrols:\n" +
		"  - id: X.1\n    title: T\n    text: body\n    citation: C\n    domain: Access control\n" +
		"    mappings:\n      - setting_path: sentinel.enabled\n        recommended: \"true\"\n        evaluator: equals\n"
}

func TestLoadProfiles(t *testing.T) {
	cases := []struct {
		name    string
		fs      fstest.MapFS
		wantErr string
		wantIDs []string
	}{
		{
			name: "ok sorted, non-yaml and dirs skipped",
			fs: fstest.MapFS{
				"p/b-two.yaml":    {Data: []byte(validProfileYAML("b-two"))},
				"p/a-one.yaml":    {Data: []byte(validProfileYAML("a-one"))},
				"p/README.md":     {Data: []byte("x")},
				"p/sub/x.yaml":    {Data: []byte("junk")},
				"p/ignored.yml.x": {Data: []byte("junk")},
			},
			wantIDs: []string{"a-one", "b-two"},
		},
		{name: "missing dir", fs: fstest.MapFS{}, wantErr: "reading p"},
		{name: "bad yaml", fs: fstest.MapFS{"p/x.yaml": {Data: []byte("id: [")}}, wantErr: "decoding"},
		{name: "unknown key", fs: fstest.MapFS{"p/x.yaml": {Data: []byte(validProfileYAML("x") + "bogus: 1\n")}}, wantErr: "bogus"},
		{name: "invalid", fs: fstest.MapFS{"p/x.yaml": {Data: []byte("id: x\n")}}, wantErr: "name and version"},
		{name: "id mismatch", fs: fstest.MapFS{"p/x.yaml": {Data: []byte(validProfileYAML("y"))}}, wantErr: "must match the file name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps, err := loadProfiles(tc.fs, "p")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, p := range ps {
				ids = append(ids, p.ID)
			}
			if !reflect.DeepEqual(ids, tc.wantIDs) {
				t.Fatalf("ids = %v, want %v", ids, tc.wantIDs)
			}
		})
	}
}

type errFS struct{ fstest.MapFS }

func (e errFS) ReadFile(name string) ([]byte, error) {
	return nil, errors.New("boom")
}

func TestLoadProfilesReadError(t *testing.T) {
	_, err := loadProfiles(errFS{fstest.MapFS{"p/x.yaml": {Data: []byte("x")}}}, "p")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want read error", err)
	}
}

func TestLoadProfilesDuplicateID(t *testing.T) {
	// Two entries can only share an ID if the file-name check passes for
	// both, which a real directory cannot produce; drive the duplicate guard
	// through a filesystem that lists the same file twice.
	dup := dupFS{fstest.MapFS{"p/a.yaml": {Data: []byte(validProfileYAML("a"))}}}
	_, err := loadProfiles(dup, "p")
	if err == nil || !strings.Contains(err.Error(), "duplicate profile id") {
		t.Fatalf("err = %v, want duplicate", err)
	}
}

type dupFS struct{ fstest.MapFS }

func (d dupFS) ReadDir(name string) ([]fs.DirEntry, error) {
	es, err := d.MapFS.ReadDir(name)
	if err != nil {
		return nil, err
	}
	return append(es, es...), nil
}

func TestProfilesLoadErrorIsSurfaced(t *testing.T) {
	origFn := profilesFn
	t.Cleanup(func() {
		profilesFn = origFn
		loadOnce = sync.Once{}
		loaded, loadErr = nil, nil
	})
	profilesFn = func() fs.FS { return fstest.MapFS{} }
	loadOnce = sync.Once{}
	if _, err := Profiles(); err == nil {
		t.Fatal("expected load error")
	}
	if _, ok := ProfileByID("soc2-type2"); ok {
		t.Fatal("ProfileByID must fail closed on load error")
	}
	r := BuildReport(&config.Config{Compliance: config.ComplianceConfig{Frameworks: []string{"soc2-type2"}}}, nil)
	if r.ProfileLoadError == "" || len(r.Controls) != 0 || !reflect.DeepEqual(r.UnknownFrameworks, []string{"soc2-type2"}) {
		t.Fatalf("report on load error = %+v", r)
	}
}

func TestProfileValidate(t *testing.T) {
	good := func() Profile {
		return Profile{ID: "x", Name: "X", Version: "1", Controls: []Control{{
			ID: "A.1", Title: "t", Text: "x", Citation: "c", Domain: "Access control",
			Mappings: []Mapping{{SettingPath: "sentinel.enabled", Recommended: "true", Evaluator: EvalEquals}},
		}}}
	}
	cases := []struct {
		name    string
		mutate  func(*Profile)
		wantErr string
	}{
		{name: "ok", mutate: func(*Profile) {}},
		{name: "no name", mutate: func(p *Profile) { p.Name = "" }, wantErr: "name and version"},
		{name: "no controls", mutate: func(p *Profile) { p.Controls = nil }, wantErr: "no controls"},
		{name: "no citation", mutate: func(p *Profile) { p.Controls[0].Citation = "" }, wantErr: "citation are required"},
		{name: "dup id", mutate: func(p *Profile) { p.Controls = append(p.Controls, p.Controls[0]) }, wantErr: "duplicate control id"},
		{name: "no text", mutate: func(p *Profile) { p.Controls[0].Text = " " }, wantErr: "text is required"},
		{name: "bad domain", mutate: func(p *Profile) { p.Controls[0].Domain = "Vibes" }, wantErr: "unknown domain"},
		{name: "not covered with mappings", mutate: func(p *Profile) {
			p.Controls[0].NotCovered = true
			p.Controls[0].Rationale = "r"
		}, wantErr: "must not have mappings"},
		{name: "not covered no rationale", mutate: func(p *Profile) {
			p.Controls[0].NotCovered = true
			p.Controls[0].Mappings = nil
		}, wantErr: "needs a rationale"},
		{name: "not covered ok", mutate: func(p *Profile) {
			p.Controls[0].NotCovered = true
			p.Controls[0].Mappings = nil
			p.Controls[0].Rationale = "physical"
		}},
		{name: "no mappings", mutate: func(p *Profile) { p.Controls[0].Mappings = nil }, wantErr: "has no mappings"},
		{name: "unknown setting", mutate: func(p *Profile) { p.Controls[0].Mappings[0].SettingPath = "nope.x" }, wantErr: "unknown setting_path"},
		{name: "unknown evaluator", mutate: func(p *Profile) { p.Controls[0].Mappings[0].Evaluator = "vibes" }, wantErr: "unknown evaluator"},
		{name: "kind mismatch", mutate: func(p *Profile) { p.Controls[0].Mappings[0].Evaluator = EvalNonEmpty }, wantErr: "cannot evaluate bool"},
		{name: "blank recommended", mutate: func(p *Profile) { p.Controls[0].Mappings[0].Recommended = " " }, wantErr: "recommended is required"},
		{name: "bad int recommended", mutate: func(p *Profile) {
			p.Controls[0].Mappings[0] = Mapping{SettingPath: "acmm_level", Recommended: "five", Evaluator: EvalAtMost}
		}, wantErr: "non-negative integer"},
		{name: "bad positive recommended", mutate: func(p *Profile) {
			p.Controls[0].Mappings[0] = Mapping{SettingPath: "dashboard.authorized_users", Recommended: "0", Evaluator: EvalMaxOwners}
		}, wantErr: "positive integer"},
		{name: "bad literal recommended", mutate: func(p *Profile) {
			p.Controls[0].Mappings[0] = Mapping{SettingPath: "auto_merge.required_checks", Recommended: "some", Evaluator: EvalNonEmpty}
		}, wantErr: `must be "non-empty"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := good()
			tc.mutate(&p)
			err := p.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
