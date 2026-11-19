package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	wfstore "github.com/hivecommons/hive/pkg/workflowmodel/store"
)

func TestStageEngineWorkflowFor(t *testing.T) {
	if stageEngineWorkflowFor(nil, nil, nil) != nil {
		t.Fatal("nil inputs")
	}
	st := wfstore.Open(t.TempDir())
	b, err := os.ReadFile("../../pkg/workflowmodel/testdata/models/techdocs.json")
	if err != nil {
		t.Fatal(err)
	}
	var m workflowmodel.Model
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Save(&m, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Project: config.ProjectConfig{Org: "cncf"}}
	if _, err := cfg.SetWorkflowBinding("techdocs", "cncf-techdocs", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.SetWorkflowBinding("ghost", "nope", "1"); err != nil {
		t.Fatal(err)
	}
	lookup := stageEngineWorkflowFor(cfg, st, discardLogger())

	if lookup("cncf", "techdocs") != nil {
		t.Fatal("flag off must resolve nothing")
	}
	cfg.Workflow.Enabled = true
	if p := lookup("cncf", "techdocs"); p == nil || p.Name != "cncf-techdocs" {
		t.Fatalf("%+v", p)
	}
	if lookup("", "techdocs") == nil {
		t.Fatal("bare repo should qualify with the org")
	}
	if lookup("cncf", "other") != nil {
		t.Fatal("unbound repo resolved a model")
	}
	// Twice: the second call exercises the warn-once path.
	for i := 0; i < 2; i++ {
		if lookup("cncf", "ghost") != nil {
			t.Fatal("missing model must fall back")
		}
	}
	if _, err := cfg.SetWorkflowBinding("techdocs", "", ""); err != nil {
		t.Fatal(err)
	}
	if lookup("cncf", "techdocs") != nil {
		t.Fatal("unbinding must take effect live")
	}
}
