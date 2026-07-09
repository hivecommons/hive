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
