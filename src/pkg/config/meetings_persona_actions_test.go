package config

import (
	"strings"
	"testing"
)

func TestMeetingsPersonaActionsDefaults(t *testing.T) {
	r := MeetingsPersonaActions{}.Resolve()
	if !r.Enabled || strings.Join(r.Types, ",") != "issue,rfc,spek,knowledge" || r.DefaultRepo != "" {
		t.Fatalf("defaults %+v", r)
	}
	for _, ty := range MeetingsActionTypes {
		if r.AutoApprove[ty] {
			t.Fatalf("%s auto-approved by default", ty)
		}
		if !r.Allows(ty) {
			t.Fatalf("%s not allowed by default", ty)
		}
	}
	if r.Allows("pr") {
		t.Fatal("unknown type allowed")
	}
	if err := (MeetingsPersonaActions{}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestMeetingsPersonaActionsResolve(t *testing.T) {
	off := false
	a := MeetingsPersonaActions{
		Enabled: &off, Types: []string{"RFC", "rfc", "bogus", "knowledge"}, DefaultRepo: "/hivecommons/hive/",
		AutoApprove: map[string]bool{"knowledge": true, "issue": true},
	}
	r := a.Resolve()
	if r.Enabled || strings.Join(r.Types, ",") != "rfc,knowledge" || r.DefaultRepo != "hivecommons/hive" {
		t.Fatalf("resolved %+v", r)
	}
	if !r.AutoApprove["knowledge"] || r.AutoApprove["issue"] {
		t.Fatalf("auto-approve must only ever hold knowledge: %+v", r.AutoApprove)
	}
	if r.Allows("rfc") {
		t.Fatal("disabled allows")
	}
}

func TestMeetingsPersonaActionsEnv(t *testing.T) {
	t.Setenv(MeetingsActionsEnabledEnv, "true")
	t.Setenv(MeetingsActionsTypesEnv, "issue, knowledge")
	t.Setenv(MeetingsActionsDefaultRepoEnv, "hivecommons/spektacular")
	t.Setenv(MeetingsActionsAutoApproveKnowledgeEnv, "yes")
	off := false
	r := MeetingsPersonaActions{Enabled: &off, Types: []string{"rfc"}, DefaultRepo: "a/b"}.Resolve()
	if !r.Enabled || strings.Join(r.Types, ",") != "issue,knowledge" || r.DefaultRepo != "hivecommons/spektacular" || !r.AutoApprove["knowledge"] {
		t.Fatalf("env %+v", r)
	}
	t.Setenv(MeetingsActionsAutoApproveKnowledgeEnv, "maybe")
	if err := (MeetingsPersonaActions{}).Validate(); err == nil || !strings.Contains(err.Error(), "not a boolean") {
		t.Fatalf("bad bool = %v", err)
	}
	t.Setenv(MeetingsActionsAutoApproveKnowledgeEnv, "")
	t.Setenv(MeetingsActionsTypesEnv, "issue,pr")
	if err := (MeetingsPersonaActions{}).Validate(); err == nil || !strings.Contains(err.Error(), `"pr"`) {
		t.Fatalf("bad env type = %v", err)
	}
	t.Setenv(MeetingsActionsTypesEnv, "")
	t.Setenv(MeetingsActionsDefaultRepoEnv, "no-slash")
	if err := (MeetingsPersonaActions{}).Validate(); err == nil || !strings.Contains(err.Error(), "owner/repo") {
		t.Fatalf("bad env repo = %v", err)
	}
}

func TestMeetingsPersonaActionsValidate(t *testing.T) {
	cases := map[string]MeetingsPersonaActions{
		"only knowledge notes": {AutoApprove: map[string]bool{"issue": true}},
		"not one of":           {AutoApprove: map[string]bool{"pr": false}},
		`"wiki"`:               {Types: []string{"issue", "wiki"}},
		"is not owner/repo":    {DefaultRepo: "hive"},
	}
	for want, a := range cases {
		if err := a.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate(%+v) = %v, want %q", a, err, want)
		}
	}
	ok := MeetingsPersonaActions{AutoApprove: map[string]bool{"knowledge": true, "issue": false, "RFC": false}, DefaultRepo: "hivecommons/hive"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	// The persona's Validate covers its actions.
	p := MeetingsPersonaConfig{Actions: MeetingsPersonaActions{AutoApprove: map[string]bool{"spek": true}}}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "auto_approve.spek") {
		t.Fatalf("persona validate = %v", err)
	}
}

func TestMeetingsActionRepos(t *testing.T) {
	var nilCfg *Config
	if nilCfg.MeetingsActionRepos() != nil || nilCfg.MeetingsActionDefaultRepo() != "" {
		t.Fatal("nil config")
	}
	c := &Config{Project: ProjectConfig{Org: "hivecommons", Repos: []string{"hive", "hivecommons/hive", "other-org/tool", "bad repo", ""}}}
	if got := strings.Join(c.MeetingsActionRepos(), ","); got != "hivecommons/hive,other-org/tool" {
		t.Fatalf("repos %q", got)
	}
	if got := c.MeetingsActionDefaultRepo(); got != "hivecommons/hive" {
		t.Fatalf("default from first repo %q", got)
	}
	c.Project.PrimaryRepo = "spektacular"
	if got := c.MeetingsActionDefaultRepo(); got != "hivecommons/spektacular" {
		t.Fatalf("default from primary %q", got)
	}
	c.Inbox.Spek.Repo = "hivecommons/inbox"
	if got := c.MeetingsActionDefaultRepo(); got != "hivecommons/inbox" {
		t.Fatalf("default from inbox.spek.repo %q", got)
	}
	c.Meetings.Persona.Actions.DefaultRepo = "hivecommons/meetings"
	if got := c.MeetingsActionDefaultRepo(); got != "hivecommons/meetings" {
		t.Fatalf("default from actions %q", got)
	}
	empty := &Config{Project: ProjectConfig{Org: "o", PrimaryRepo: "p"}}
	if got := strings.Join(empty.MeetingsActionRepos(), ","); got != "o/p" {
		t.Fatalf("primary fallback %q", got)
	}
	none := &Config{}
	if none.MeetingsActionDefaultRepo() != "" {
		t.Fatal("no repos, no default")
	}
}
