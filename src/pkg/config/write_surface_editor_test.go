package config

import (
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

// #9587 phase 2: the dashboard allowlist editor's config side.

func TestNormalizeWriteSurfaceAllowlist_EmptyIsNoAllowlist(t *testing.T) {
	for _, in := range []map[string][]string{nil, {}} {
		out, err := NormalizeWriteSurfaceAllowlist(in)
		if err != nil || out != nil {
			t.Fatalf("Normalize(%v) = %v, %v; want nil, nil (nothing restricted)", in, out, err)
		}
	}
}

func TestNormalizeWriteSurfaceAllowlist_Canonical(t *testing.T) {
	out, err := NormalizeWriteSurfaceAllowlist(map[string][]string{
		" scanner ": {"Comment", " create_issue", "comment", "", "claim"},
		"muted":     {},
		"nil-list":  nil,
		"operator":  {"merge_pr", "*", "comment"},
		"reviewer":  {"resolve_thread", "review"},
	})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	want := map[string][]string{
		"scanner":  {"create_issue", "comment", "claim"}, // KnownWriteOps order, deduped, lower-cased
		"muted":    {},                                   // allow-nothing survives as an empty list
		"nil-list": {},
		"operator": {WriteSurfaceAllowAll}, // "*" collapses the list
		"reviewer": {"review", "resolve_thread"},
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("Normalize = %#v\nwant %#v", out, want)
	}
	for lane, ops := range out {
		if ops == nil {
			t.Errorf("lane %q normalized to nil; an empty list must stay distinct from no entry", lane)
		}
	}
}

func TestNormalizeWriteSurfaceAllowlist_Rejects(t *testing.T) {
	tooMany := map[string][]string{}
	for i := 0; i <= WriteSurfaceMaxLanes; i++ {
		tooMany["lane"+strconv.Itoa(i)] = nil
	}
	cases := []struct {
		name    string
		in      map[string][]string
		wantErr string
	}{
		{"unknown op", map[string][]string{"scanner": {"comment", "delete_repo"}}, `unknown operation "delete_repo"`},
		{"empty lane", map[string][]string{"  ": {"comment"}}, "lane name is empty"},
		{"markup lane", map[string][]string{"<b>x</b>": {"comment"}}, "may contain only"},
		{"space in lane", map[string][]string{"scan ner": {"comment"}}, "may contain only"},
		{"path lane", map[string][]string{"../x": {"comment"}}, "may contain only"},
		{"long lane", map[string][]string{strings.Repeat("a", WriteSurfaceMaxLaneNameLen+1): {"comment"}}, "longer than"},
		{"duplicate after trim", map[string][]string{"scanner": {"comment"}, " scanner": {"claim"}}, "listed twice"},
		{"too many lanes", tooMany, "the limit is"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := NormalizeWriteSurfaceAllowlist(tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if out != nil {
				t.Fatalf("a rejected allowlist must return nothing, got %v", out)
			}
		})
	}
}

func TestSetWriteSurfaceAllowlist_TakesEffectAndCopies(t *testing.T) {
	cfg := &Config{}
	in := map[string][]string{"scanner": {"comment"}}
	cfg.SetWriteSurfaceAllowlist(in)

	if cfg.AgentMayWrite("scanner", "merge_pr") || !cfg.AgentMayWrite("scanner", "comment") {
		t.Fatal("the new allowlist must be in force immediately")
	}
	if !cfg.AgentMayWrite("reviewer", "merge_pr") {
		t.Fatal("a lane with no entry must stay unrestricted")
	}

	// The caller's map is copied in, and the getter copies out.
	in["scanner"][0] = "merge_pr"
	if cfg.AgentMayWrite("scanner", "merge_pr") {
		t.Fatal("mutating the caller's map changed the live allowlist")
	}
	got := cfg.WriteSurfaceAllowlist()
	got["scanner"] = append(got["scanner"], "merge_pr")
	got["intruder"] = []string{}
	if cfg.AgentMayWrite("scanner", "merge_pr") || !cfg.AgentMayWrite("intruder", "comment") {
		t.Fatal("mutating the getter's copy changed the live allowlist")
	}

	cfg.SetWriteSurfaceAllowlist(nil)
	if cfg.WriteSurfaceAllowlist() != nil || !cfg.AgentMayWrite("scanner", "merge_pr") {
		t.Fatal("clearing the allowlist must restore unrestricted behaviour")
	}

	var nilCfg *Config
	nilCfg.SetWriteSurfaceAllowlist(in)
	if nilCfg.WriteSurfaceAllowlist() != nil {
		t.Fatal("nil config must read as no allowlist")
	}
}

// An empty (allow-nothing) lane must survive the persisted YAML round trip,
// or a restart would silently turn "allow nothing" into "unrestricted".
func TestWriteSurfaceAllowlist_YAMLRoundTripKeepsEmptyLane(t *testing.T) {
	cfg := &Config{}
	cfg.SetWriteSurfaceAllowlist(map[string][]string{"muted": {}, "scanner": {"comment"}})
	data, err := yaml.Marshal(cfg.WriteSurface)
	if err != nil {
		t.Fatal(err)
	}
	var back WriteSurfaceConfig
	if err := yaml.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if ops, ok := back.Allowlist["muted"]; !ok || len(ops) != 0 {
		t.Fatalf("muted lane after round trip = %v (present=%v); yaml:\n%s", ops, ok, data)
	}
	reloaded := &Config{WriteSurface: back}
	if reloaded.AgentMayWrite("muted", "comment") {
		t.Fatal("an allow-nothing lane became unrestricted after a reload")
	}
}

// The relays read the allowlist on their own goroutines while the dashboard
// replaces it; under -race this fails if either side skips the lock.
func TestWriteSurfaceAllowlist_ConcurrentReadWrite(t *testing.T) {
	cfg := &Config{}
	const rounds = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			cfg.SetWriteSurfaceAllowlist(map[string][]string{"scanner": {"comment"}})
			cfg.SetWriteSurfaceAllowlist(nil)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			_ = cfg.AgentMayWrite("scanner", "comment")
			_ = cfg.WriteSurfaceAllowlist()
			_ = WriteSurfaceWarnings(cfg)
		}
	}()
	wg.Wait()
}
