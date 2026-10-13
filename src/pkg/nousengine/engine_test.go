package nousengine

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/nousstore"
)

func TestRegistryDefaultIsNous(t *testing.T) {
	names := Names()
	for _, want := range []string{config.ExperimentEngineNous, FakeName} {
		found := false
		for _, n := range names {
			found = found || n == want
		}
		if !found {
			t.Fatalf("Names() = %v, missing %q", names, want)
		}
	}
	for _, name := range []string{"", "nous", " Nous "} {
		e, err := New(name, Settings{})
		if err != nil {
			t.Fatalf("New(%q): %v", name, err)
		}
		if e.Name() != config.ExperimentEngineNous || e.Dispatcher() != config.NousDispatcherInline {
			t.Fatalf("New(%q) = %s/%s, want nous/inline", name, e.Name(), e.Dispatcher())
		}
	}
	if _, err := New("mlflow", Settings{}); !errors.Is(err, ErrUnknownEngine) || !strings.Contains(err.Error(), "registered: ") {
		t.Fatalf("New(mlflow) = %v, want ErrUnknownEngine listing registered engines", err)
	}
}

func TestRegisterTwicePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Register twice did not panic")
		}
	}()
	Register(FakeName, func(Settings) (Engine, error) { return nil, nil })
}

func TestNewNousDispatcher(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", config.NousDispatcherInline},
		{"inline", config.NousDispatcherInline},
		{" SDK ", config.NousDispatcherSDK},
	} {
		n, err := NewNous(Settings{Dispatcher: tc.in})
		if err != nil || n.Dispatcher() != tc.want {
			t.Fatalf("NewNous(%q) = %v/%v, want %q", tc.in, n, err, tc.want)
		}
	}
	if _, err := NewNous(Settings{Dispatcher: "legacy"}); err == nil {
		t.Fatal("NewNous(legacy) = nil error, want invalid dispatcher")
	}
}

func TestNousInvocation(t *testing.T) {
	n, err := NewNous(Settings{Root: "/srv/nous", Dispatcher: "sdk", BinDir: "/opt/bin", CampaignDir: "/opt/campaigns", FetchAllowlist: []string{"github.com", "huggingface.co"}})
	if err != nil {
		t.Fatal(err)
	}
	inv, err := n.Invocation(Campaign{ID: "exp-1", Mode: "suggest", Scope: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	wantArgv := []string{"bash", "/opt/bin/" + RunnerScript}
	if !reflect.DeepEqual(inv.Argv, wantArgv) {
		t.Fatalf("Argv = %v, want %v", inv.Argv, wantArgv)
	}
	for _, want := range []string{
		"NOUS_AGENT=sdk",
		config.NousDispatcherEnv + "=sdk",
		config.NousEngineEnv + "=nous",
		"NOUS_DATA_DIR=/srv/nous",
		nousstore.EnvRoot + "=/srv/nous",
		"NOUS_CAMPAIGN_PATH=/opt/campaigns/" + DefaultCampaignFile,
		"NOUS_RUN_ID=exp-1",
		"NOUS_HIVE_MODE=suggest",
		"NOUS_HIVE_SCOPE=repo",
		config.NousFetchAllowlistEnv + "=github.com,huggingface.co",
	} {
		if !contains(inv.Env, want) {
			t.Errorf("Env missing %q: %v", want, inv.Env)
		}
	}

	// Defaults: inline, image install paths, durable root; no optional vars.
	def, _ := NewNous(Settings{})
	inv, err = def.Invocation(Campaign{})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Argv[1] != filepath.Join(DefaultBinDir, RunnerScript) {
		t.Fatalf("default runner = %q", inv.Argv[1])
	}
	if !contains(inv.Env, "NOUS_AGENT=inline") || !contains(inv.Env, "NOUS_DATA_DIR="+nousstore.DefaultRoot) {
		t.Fatalf("default env = %v", inv.Env)
	}
	for _, e := range inv.Env {
		if strings.HasPrefix(e, "NOUS_RUN_ID=") || strings.HasPrefix(e, "NOUS_HIVE_MODE=") {
			t.Fatalf("optional var exported for empty campaign: %v", inv.Env)
		}
	}
	if _, err := def.Invocation(Campaign{DefinitionPath: "relative.yaml"}); err == nil {
		t.Fatal("relative DefinitionPath accepted")
	}
}

func TestNousLayout(t *testing.T) {
	n, _ := NewNous(Settings{Root: "/srv/nous"})
	l := n.Layout(Campaign{ID: "exp-1"})
	want := Layout{
		Root:         "/srv/nous/exp-1",
		Ledger:       "/srv/nous/exp-1/" + nousstore.LedgerFile,
		Principles:   "/srv/nous/exp-1/" + nousstore.PrinciplesFile,
		Verdict:      "/srv/nous/exp-1/" + MetaFindingsFile,
		GatesDir:     nousstore.New("/srv/nous").GatesDir(),
		ArtifactsDir: "/srv/nous/exp-1/" + RunsSubdir,
	}
	if l != want {
		t.Fatalf("Layout = %+v, want %+v", l, want)
	}
	if got := n.Layout(Campaign{}).Root; got != "/srv/nous/"+nousstore.GovernorSubdir {
		t.Fatalf("empty campaign root = %q", got)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNousProgressAndPrinciples(t *testing.T) {
	root := t.TempDir()
	n, _ := NewNous(Settings{Root: root})
	c := Campaign{ID: "exp"}

	rounds, err := n.Progress(c)
	if err != nil || len(rounds) != 0 {
		t.Fatalf("missing ledger: %v %v", rounds, err)
	}
	ps, err := n.Principles(c)
	if err != nil || len(ps) != 0 {
		t.Fatalf("missing principles: %v %v", ps, err)
	}

	writeFile(t, n.Layout(c).Ledger, `{"iterations":[{"ts":"t0","type":"probe","status":"ok","hypothesis":"h0"},{"timestamp":"t1","summary":"s1"}]}`)
	rounds, err = n.Progress(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 || rounds[0].Index != 0 || rounds[0].Summary != "h0" || rounds[0].Type != "probe" ||
		rounds[1].Index != 1 || rounds[1].Timestamp != "t1" || rounds[1].Summary != "s1" {
		t.Fatalf("rounds = %+v", rounds)
	}

	writeFile(t, n.Layout(c).Principles, `{"principles":[
		{"id":"p1","statement":"a","confidence":"high","category":"cat","evidence":["e1"]},
		{"text":"b","confidence":0.33,"citations":["c1","c2"]},
		{"statement":"c","confidence":"weird"}]}`)
	ps, err = n.Principles(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 3 || ps[0].ID != "p1" || ps[0].Text != "a" || ps[0].Confidence != confidenceHigh || ps[0].Source != "cat" || !reflect.DeepEqual(ps[0].Evidence, []string{"e1"}) ||
		ps[1].Text != "b" || ps[1].Confidence != 0.33 || len(ps[1].Evidence) != 2 ||
		ps[2].Confidence != confidenceUnknown {
		t.Fatalf("principles = %+v", ps)
	}

	writeFile(t, n.Layout(c).Ledger, `{not json`)
	if _, err := n.Progress(c); err == nil {
		t.Fatal("corrupt ledger accepted")
	}
	writeFile(t, n.Layout(c).Principles, `{not json`)
	if _, err := n.Principles(c); err == nil {
		t.Fatal("corrupt principles accepted")
	}
}

func TestNousVerdict(t *testing.T) {
	root := t.TempDir()
	n, _ := NewNous(Settings{Root: root})
	c := Campaign{ID: "exp"}
	path := n.Layout(c).Verdict

	if _, err := n.Verdict(c); !errors.Is(err, ErrNoVerdict) {
		t.Fatalf("missing file: %v, want ErrNoVerdict", err)
	}
	writeFile(t, path, `{"summary":"running"}`)
	if _, err := n.Verdict(c); !errors.Is(err, ErrNoVerdict) {
		t.Fatalf("no recommendation: %v, want ErrNoVerdict", err)
	}
	writeFile(t, path, `{"deployment_recommendation":"fallback"}`)
	v, err := n.Verdict(c)
	if err != nil || v.Recommendation != VerdictFallBack || v.MeasuredAgainstBaseline {
		t.Fatalf("string verdict = %+v, %v", v, err)
	}
	writeFile(t, path, `{"deployment_recommendation":{"recommendation":"DEPLOY","composite_score":0.82,"baseline_score":0.7,"citations":["ledger#3"],"caveats":["n=2"],"rationale":"better"}}`)
	v, err = n.Verdict(c)
	if err != nil {
		t.Fatal(err)
	}
	if v.Recommendation != VerdictDeploy || v.Score != 0.82 || v.Baseline != 0.7 || !v.MeasuredAgainstBaseline ||
		v.Summary != "better" || !reflect.DeepEqual(v.Citations, []string{"ledger#3"}) || !reflect.DeepEqual(v.Caveats, []string{"n=2"}) {
		t.Fatalf("object verdict = %+v", v)
	}
	writeFile(t, path, `{"deployment_recommendation":{"decision":"deploy_with_caveats","score":"0.5","measured_against_baseline":false}}`)
	v, err = n.Verdict(c)
	if err != nil || v.Recommendation != VerdictWithCaveats || v.Score != 0.5 || v.MeasuredAgainstBaseline {
		t.Fatalf("explicit baseline flag verdict = %+v, %v", v, err)
	}
	writeFile(t, path, `{"deployment_recommendation":{"score":1}}`)
	if _, err := n.Verdict(c); !errors.Is(err, ErrNoVerdict) {
		t.Fatalf("object without label: %v, want ErrNoVerdict", err)
	}
	writeFile(t, path, `{"deployment_recommendation":42}`)
	if _, err := n.Verdict(c); err == nil || errors.Is(err, ErrNoVerdict) {
		t.Fatalf("unexpected type: %v", err)
	}
	writeFile(t, path, `{nope`)
	if _, err := n.Verdict(c); err == nil {
		t.Fatal("corrupt meta_findings accepted")
	}
}

func TestFakeRecordsCalls(t *testing.T) {
	e, err := New(FakeName, Settings{Dispatcher: "sdk"})
	if err != nil {
		t.Fatal(err)
	}
	f, ok := e.(*Fake)
	if !ok || f.Dispatcher() != "sdk" || f.Name() != FakeName {
		t.Fatalf("New(fake) = %T %v", e, e)
	}
	if NewFake("").Dispatcher() != config.DefaultNousDispatcher {
		t.Fatal("NewFake(\"\") dispatcher not defaulted")
	}
	c := Campaign{ID: "x"}
	if _, err := f.Verdict(c); !errors.Is(err, ErrNoVerdict) {
		t.Fatalf("unconfigured Fake verdict = %v", err)
	}
	f.VerdictResult, f.VerdictErr = Verdict{Recommendation: VerdictDeploy}, nil
	f.ProgressResult = []Round{{Index: 0}}
	f.PrinciplesResult = []Principle{{Text: "p"}}
	f.LayoutResult = Layout{Root: "/r"}
	f.InvocationResult = Invocation{Argv: []string{"true"}}
	if v, _ := f.Verdict(c); v.Recommendation != VerdictDeploy {
		t.Fatal("VerdictResult not returned")
	}
	if r, _ := f.Progress(c); len(r) != 1 {
		t.Fatal("ProgressResult not returned")
	}
	if p, _ := f.Principles(c); len(p) != 1 {
		t.Fatal("PrinciplesResult not returned")
	}
	if f.Layout(c).Root != "/r" {
		t.Fatal("LayoutResult not returned")
	}
	if inv, _ := f.Invocation(c); inv.Argv[0] != "true" {
		t.Fatal("InvocationResult not returned")
	}
	if got := f.CallsTo("Verdict"); len(got) != 2 || got[0].ID != "x" {
		t.Fatalf("CallsTo(Verdict) = %v", got)
	}
	if len(f.Calls) != 6 {
		t.Fatalf("Calls = %d, want 6", len(f.Calls))
	}
}

func TestHiveEnv(t *testing.T) {
	got := HiveEnv(Settings{})
	want := map[string]string{
		config.NousDispatcherEnv:     config.DefaultNousDispatcher,
		nousstore.EnvRoot:            nousstore.DefaultRoot,
		config.NousFetchAllowlistEnv: strings.Join(config.DefaultNousFetchAllowlist, config.NousFetchAllowlistSeparator),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("HiveEnv(empty) = %v, want %v", got, want)
	}
	// An explicitly empty allowlist (deny all) must not export a blank value,
	// which the subcommand would read as "unset → default".
	if got = HiveEnv(Settings{FetchAllowlist: []string{}}); got[config.NousFetchAllowlistEnv] != config.NousFetchAllowlistSeparator {
		t.Fatalf("deny-all allowlist exported as %q", got[config.NousFetchAllowlistEnv])
	}
	if got = HiveEnv(Settings{FetchAllowlist: []string{"mirror.example"}}); got[config.NousFetchAllowlistEnv] != "mirror.example" {
		t.Fatalf("allowlist exported as %q", got[config.NousFetchAllowlistEnv])
	}
	got = HiveEnv(Settings{Root: " /srv/nous ", Dispatcher: "SDK"})
	if got[config.NousDispatcherEnv] != config.NousDispatcherSDK || got[nousstore.EnvRoot] != "/srv/nous" {
		t.Fatalf("HiveEnv = %v", got)
	}
	if got = HiveEnv(Settings{Dispatcher: "legacy"}); got[config.NousDispatcherEnv] != config.DefaultNousDispatcher {
		t.Fatalf("unknown dispatcher not defaulted: %v", got)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
