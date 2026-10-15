package config

import (
	"testing"
	"time"
)

func TestNousGatePollInterval(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		yaml int
		want time.Duration
	}{
		{"default", "", 0, DefaultNousGatePollInterval},
		{"yaml", "", 45, 45 * time.Second},
		{"negative yaml falls back", "", -3, DefaultNousGatePollInterval},
		{"env duration wins", "2m", 45, 2 * time.Minute},
		{"env seconds wins", "7", 45, 7 * time.Second},
		{"env zero ignored", "0", 45, 45 * time.Second},
		{"env garbage ignored", "soon", 0, DefaultNousGatePollInterval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(NousGatePollIntervalEnv, tc.env)
			if got := (NousConfig{GatePollSeconds: tc.yaml}).GatePollInterval(); got != tc.want {
				t.Fatalf("GatePollInterval() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNousResolveFetchAllowlist(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == NousFetchAllowlistEnv {
				return v
			}
			return ""
		}
	}
	for _, tc := range []struct {
		name string
		env  string
		yaml []string
		want []string
	}{
		{"default when nil", "", nil, []string{"github.com"}},
		{"explicit empty yaml denies all", "", []string{}, []string{}},
		{"yaml wins over default", "", []string{"github.com", "huggingface.co"}, []string{"github.com", "huggingface.co"}},
		{"yaml normalised and deduped", "", []string{" GitHub.com ", "github.com", ""}, []string{"github.com"}},
		{"env wins over yaml", "huggingface.co, Objects.githubusercontent.com", []string{"github.com"}, []string{"huggingface.co", "objects.githubusercontent.com"}},
		{"blank env falls through", "   ", []string{"example.org"}, []string{"example.org"}},
		{"env of only separators yields empty", ",,", nil, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := (NousConfig{FetchAllowlist: tc.yaml}).ResolveFetchAllowlist(env(tc.env))
			if len(got) != len(tc.want) {
				t.Fatalf("ResolveFetchAllowlist() = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ResolveFetchAllowlist()[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
	t.Run("nil getenv skips env tier", func(t *testing.T) {
		t.Setenv(NousFetchAllowlistEnv, "evil.example")
		if got := (NousConfig{}).ResolveFetchAllowlist(nil); len(got) != 1 || got[0] != "github.com" {
			t.Fatalf("ResolveFetchAllowlist(nil) = %q, want default", got)
		}
	})
	t.Run("ResolvedFetchAllowlist reads the process env", func(t *testing.T) {
		t.Setenv(NousFetchAllowlistEnv, "mirror.example")
		if got := (NousConfig{}).ResolvedFetchAllowlist(); len(got) != 1 || got[0] != "mirror.example" {
			t.Fatalf("ResolvedFetchAllowlist() = %q", got)
		}
	})
}

func TestNousValidateFetchAllowlist(t *testing.T) {
	for _, tc := range []struct {
		host string
		ok   bool
	}{
		{"github.com", true},
		{"objects.githubusercontent.com", true},
		{"", false},
		{"GitHub.com", false},
		{"*.github.com", false},
		{"https://github.com", false},
		{"github.com/owner", false},
		{"github.com:443", false},
		{"127.0.0.1", false},
		{"::1", false},
		{"localhost", false},
		{"svc.localhost", false},
		{"user@github.com", false},
	} {
		t.Run(tc.host, func(t *testing.T) {
			err := (NousConfig{FetchAllowlist: []string{tc.host}}).Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate(%q) = %v, want nil", tc.host, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("Validate(%q) = nil, want error", tc.host)
			}
		})
	}
	if err := (NousConfig{}).Validate(); err != nil {
		t.Fatalf("empty config: %v", err)
	}
}

func TestNousDispatcherValuesAndCanonical(t *testing.T) {
	vals := NousDispatcherValues()
	if len(vals) != len(ValidNousDispatchers) || vals[0] != DefaultNousDispatcher {
		t.Fatalf("NousDispatcherValues() = %v, want default first and one entry per valid dispatcher", vals)
	}
	seen := map[string]bool{}
	for _, v := range vals {
		if !ValidNousDispatchers[v] || seen[v] {
			t.Fatalf("NousDispatcherValues() = %v contains an unknown or duplicate entry %q", vals, v)
		}
		seen[v] = true
	}
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", true},
		{"  ", "", true},
		{"inline", "inline", true},
		{" SDK ", "sdk", true},
		{"carrier-pigeon", "carrier-pigeon", false},
	} {
		got, ok := CanonicalNousDispatcher(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("CanonicalNousDispatcher(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
