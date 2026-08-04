package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAttributionTrailerDefaultOn(t *testing.T) {
	// nil (key omitted) → enabled: an untouched hive stamps the trailer.
	var g GovernorConfig
	if !g.AttributionTrailerEnabled() {
		t.Error("omitted governor.attribution_trailer must default to on")
	}
	// explicit false → disabled (trailer only; audit is not gated here).
	f := false
	g.AttributionTrailer = &f
	if g.AttributionTrailerEnabled() {
		t.Error("explicit attribution_trailer=false must disable the trailer")
	}
	// explicit true → enabled
	tr := true
	g.AttributionTrailer = &tr
	if !g.AttributionTrailerEnabled() {
		t.Error("explicit attribution_trailer=true must enable the trailer")
	}
}

func TestAttributionTrailerYAMLParsing(t *testing.T) {
	// Absent key parses to nil → default ON.
	var g GovernorConfig
	if err := yaml.Unmarshal([]byte("eval_interval_s: 30\n"), &g); err != nil {
		t.Fatal(err)
	}
	if g.AttributionTrailer != nil || !g.AttributionTrailerEnabled() {
		t.Errorf("absent key: pointer=%v enabled=%v, want nil/true", g.AttributionTrailer, g.AttributionTrailerEnabled())
	}
	// Explicit false round-trips as an explicit false, not as "unset".
	g = GovernorConfig{}
	if err := yaml.Unmarshal([]byte("attribution_trailer: false\n"), &g); err != nil {
		t.Fatal(err)
	}
	if g.AttributionTrailer == nil || *g.AttributionTrailer || g.AttributionTrailerEnabled() {
		t.Errorf("explicit false not honored: pointer=%v", g.AttributionTrailer)
	}
	// Explicit true.
	g = GovernorConfig{}
	if err := yaml.Unmarshal([]byte("attribution_trailer: true\n"), &g); err != nil {
		t.Fatal(err)
	}
	if g.AttributionTrailer == nil || !*g.AttributionTrailer || !g.AttributionTrailerEnabled() {
		t.Errorf("explicit true not honored: pointer=%v", g.AttributionTrailer)
	}
}
