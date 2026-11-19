package config

import (
	"strings"
	"testing"
)

func TestWorkflowBindings(t *testing.T) {
	var nilCfg *Config
	if _, ok := nilCfg.WorkflowBindingFor("x"); ok {
		t.Fatal("nil config bound")
	}
	if _, err := nilCfg.SetWorkflowBinding("x", "a", "1"); err == nil {
		t.Fatal("nil config set")
	}
	c := &Config{Project: ProjectConfig{Org: "hivecommons"}}
	if _, ok := c.WorkflowBindingFor(""); ok {
		t.Fatal("empty repo bound")
	}
	if _, err := c.SetWorkflowBinding(" ", "a", "1"); err == nil {
		t.Fatal("empty repo accepted")
	}
	if _, err := c.SetWorkflowBinding("docs", "a", ""); err == nil {
		t.Fatal("missing version accepted")
	}
	changed, err := c.SetWorkflowBinding("docs", "cncf-techdocs", "1")
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	b, ok := c.WorkflowBindingFor("HiveCommons/Docs")
	if !ok || b.Key() != "cncf-techdocs@1" {
		t.Fatalf("%+v %v", b, ok)
	}
	if changed, _ := c.SetWorkflowBinding("hivecommons/docs", "cncf-techdocs", "1"); changed {
		t.Fatal("same binding reported changed")
	}
	if changed, _ := c.SetWorkflowBinding("docs", "cncf-techdocs", "2"); !changed || len(c.Project.WorkflowBindings) != 1 {
		t.Fatalf("%+v", c.Project.WorkflowBindings)
	}
	if changed, _ := c.SetWorkflowBinding("other", "", ""); changed {
		t.Fatal("unbinding an unbound repo changed")
	}
	if changed, _ := c.SetWorkflowBinding("docs", "", ""); !changed || len(c.Project.WorkflowBindings) != 0 {
		t.Fatalf("%+v", c.Project.WorkflowBindings)
	}
}

func TestValidateWorkflowBindings(t *testing.T) {
	cases := []struct {
		bs   []WorkflowBinding
		want string
	}{
		{nil, ""},
		{[]WorkflowBinding{{Repo: "a", Name: "m", Version: "1"}}, ""},
		{[]WorkflowBinding{{Name: "m", Version: "1"}}, "repo is required"},
		{[]WorkflowBinding{{Repo: "a", Name: "m"}}, "name and version"},
		{[]WorkflowBinding{{Repo: "a", Name: "m", Version: "1"}, {Repo: "A", Name: "n", Version: "1"}}, "bound twice"},
	}
	for _, tc := range cases {
		err := validateWorkflowBindings(tc.bs)
		if tc.want == "" && err != nil {
			t.Fatalf("%+v: %v", tc.bs, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Fatalf("%+v: %v", tc.bs, err)
		}
	}
}
