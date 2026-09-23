package config

import "testing"

func TestWavefrontSourceConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     WavefrontSourceConfig
		wantErr bool
	}{
		{name: "disabled is always valid", cfg: WavefrontSourceConfig{}},
		{name: "disabled with junk is valid", cfg: WavefrontSourceConfig{Path: "x", URL: "y"}},
		{name: "enabled needs a location", cfg: WavefrontSourceConfig{Enabled: true, Repo: "acme/repo"}, wantErr: true},
		{name: "path and url exclusive", cfg: WavefrontSourceConfig{Enabled: true, Path: "g.json", URL: "https://x", Repo: "acme/repo"}, wantErr: true},
		{name: "url scheme", cfg: WavefrontSourceConfig{Enabled: true, URL: "ftp://x", Repo: "acme/repo"}, wantErr: true},
		{name: "repo required", cfg: WavefrontSourceConfig{Enabled: true, Path: "g.json"}, wantErr: true},
		{name: "path ok", cfg: WavefrontSourceConfig{Enabled: true, Path: "g.json", Repo: "acme/repo"}},
		{name: "url ok", cfg: WavefrontSourceConfig{Enabled: true, URL: "https://wavefront.example/graph.json", Repo: "acme/repo"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestWorkSourceConfigIsZeroSeesWavefront(t *testing.T) {
	if !(WorkSourceConfig{}).IsZero() {
		t.Fatal("empty work source should be zero")
	}
	if (WorkSourceConfig{Wavefront: WavefrontSourceConfig{Enabled: true}}).IsZero() {
		t.Fatal("wavefront block must make the work source non-zero")
	}
}
