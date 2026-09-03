package hub

import "testing"

// TestNextLinkURL pins nextLinkURL's parsing of registry Link headers: the
// rel="next" selection, GHCR relative-path resolution against ghcrBase, and
// the malformed-bracket bail-outs.
func TestNextLinkURL(t *testing.T) {
	cases := []struct {
		name string
		link string
		want string
	}{
		{
			name: "relative GHCR path resolved against registry host",
			link: `</v2/hivecommons/hive/tags/list?last=v4-abc&n=1000>; rel="next"`,
			want: ghcrBase + "/v2/hivecommons/hive/tags/list?last=v4-abc&n=1000",
		},
		{
			name: "absolute URL returned verbatim",
			link: `<https://registry.example.com/v2/tags/list?last=x>; rel="next"`,
			want: "https://registry.example.com/v2/tags/list?last=x",
		},
		{
			name: "next selected among multiple comma-separated parts",
			link: `<https://x.example/prev>; rel="prev", </v2/list?last=y>; rel="next"`,
			want: ghcrBase + "/v2/list?last=y",
		},
		{
			name: "no rel=next part",
			link: `<https://x.example/prev>; rel="prev"`,
			want: "",
		},
		{
			name: "empty header",
			link: "",
			want: "",
		},
		{
			name: "rel=next without angle brackets",
			link: `/v2/list?last=z; rel="next"`,
			want: "",
		},
		{
			name: "closing bracket before opening bracket",
			link: `>oops<; rel="next"`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextLinkURL(tc.link); got != tc.want {
				t.Fatalf("nextLinkURL(%q) = %q, want %q", tc.link, got, tc.want)
			}
		})
	}
}

// TestSessionCookieParentDomain pins the derivation chain of
// sessionCookieParentDomain: publicsuffix eTLD+1 for real domains, then the
// strip-first-label fallback, then the host itself when it has no dot.
func TestSessionCookieParentDomain(t *testing.T) {
	cases := []struct {
		name   string
		hubURL string // HIVE_HUB_PUBLIC_URL value; "" = historical default
		want   string
	}{
		{
			name:   "historical default hub host",
			hubURL: "",
			want:   "kubestellar.io",
		},
		{
			name:   "configured hub host uses its registrable parent",
			hubURL: "https://hive.hivecommons.dev/",
			want:   "hivecommons.dev",
		},
		{
			name:   "dotless host falls back to the host itself",
			hubURL: "http://localhost:8080",
			want:   "localhost",
		},
		{
			name:   "non-suffix dotted host strips the first label",
			hubURL: "http://127.0.0.1:8080",
			want:   "0.0.1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HIVE_HUB_PUBLIC_URL", tc.hubURL)
			if got := sessionCookieParentDomain(); got != tc.want {
				t.Fatalf("sessionCookieParentDomain() with hub %q = %q, want %q",
					tc.hubURL, got, tc.want)
			}
		})
	}
}
