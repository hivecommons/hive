package config

import (
	"reflect"
	"testing"
)

func TestParseAuthorizedUsers(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"clubanderson", []string{"clubanderson"}},
		{"clubanderson,kellyaa", []string{"clubanderson", "kellyaa"}},
		{" clubanderson , kellyaa ", []string{"clubanderson", "kellyaa"}},
		{"clubanderson,,kellyaa,", []string{"clubanderson", "kellyaa"}},
		{"", []string{}},
	}
	for _, c := range cases {
		got := parseAuthorizedUsers(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseAuthorizedUsers(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestApplyBootstrapEnv_ReadsAuthorizedUsers(t *testing.T) {
	t.Setenv("HIVE_AUTHORIZED_USERS", "clubanderson,kellyaa")
	c := &Config{}
	c.applyBootstrapEnv()
	if len(c.Dashboard.AuthorizedUsers) != 2 ||
		c.Dashboard.AuthorizedUsers[0] != "clubanderson" ||
		c.Dashboard.AuthorizedUsers[1] != "kellyaa" {
		t.Fatalf("expected authorized users from env, got %v", c.Dashboard.AuthorizedUsers)
	}
	if !c.Dashboard.IsDirectRouteAuthzEnabled() {
		t.Fatal("direct-route authz should be enabled when list is populated")
	}
}

func TestApplyBootstrapEnv_ConfigListWins(t *testing.T) {
	t.Setenv("HIVE_AUTHORIZED_USERS", "envuser")
	c := &Config{}
	c.Dashboard.AuthorizedUsers = []string{"yamluser"}
	c.applyBootstrapEnv()
	if len(c.Dashboard.AuthorizedUsers) != 1 || c.Dashboard.AuthorizedUsers[0] != "yamluser" {
		t.Fatalf("explicit config list must win over env, got %v", c.Dashboard.AuthorizedUsers)
	}
}

// TestIsDirectRouteAuthzEnabled_DecoupledFromHubProxied locks in the fix for
// hub-proxied hives being wrongly forced into standalone direct-route mode.
// direct-route authz must be on ONLY for a standalone spoke (allowlist AND not
// hub-proxied); a hub-proxied hive keeps nginx as its gate even with a list.
func TestIsDirectRouteAuthzEnabled_DecoupledFromHubProxied(t *testing.T) {
	cases := []struct {
		name       string
		users      []string
		hubProxied bool
		want       bool
	}{
		{"standalone spoke with allowlist -> direct-route", []string{"clubanderson:owner"}, false, true},
		{"hub-proxied with allowlist -> NOT direct-route (nginx gates)", []string{"clubanderson:owner", "MikeSpreitzer:read"}, true, false},
		{"hub-proxied, no allowlist", nil, true, false},
		{"standalone, no allowlist (misconfigured/hub-proxied)", nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := DashboardConfig{AuthorizedUsers: tc.users, HubProxied: tc.hubProxied}
			if got := d.IsDirectRouteAuthzEnabled(); got != tc.want {
				t.Errorf("IsDirectRouteAuthzEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}
