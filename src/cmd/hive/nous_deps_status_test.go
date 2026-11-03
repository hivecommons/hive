package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/nousdeps"
	"github.com/hivecommons/hive/pkg/nousstore"
)

func TestPublishNousDependencyStatus(t *testing.T) {
	publishNousDependencyStatus(nil, nousstore.New("/srv/nous"), nil) // must not panic

	state := &dashboard.NousState{Status: map[string]interface{}{"phase": "idle"}}
	publishNousDependencyStatus(state, nousstore.New("/srv/nous"), []string{"github.com", "huggingface.co"})
	if state.Status["phase"] != "idle" {
		t.Fatal("existing status keys must be preserved")
	}
	deps, ok := state.Status[nousStatusDependenciesKey].(map[string]interface{})
	if !ok {
		t.Fatalf("status[%q] = %#v", nousStatusDependenciesKey, state.Status[nousStatusDependenciesKey])
	}
	if got := deps["fetch_allowlist"].([]string); len(got) != 2 || got[1] != "huggingface.co" {
		t.Fatalf("fetch_allowlist = %v", got)
	}
	if deps["cache_dir"] != "/srv/nous/"+nousdeps.CacheSubdir || deps["record_file"] != "/srv/nous/governor/"+nousdeps.RecordFile {
		t.Fatalf("paths = %v", deps)
	}

	// A nil Status map and a deny-all allowlist are both representable.
	empty := &dashboard.NousState{}
	publishNousDependencyStatus(empty, nousstore.New("/srv/nous"), []string{})
	if got := empty.Status[nousStatusDependenciesKey].(map[string]interface{})["fetch_allowlist"].([]string); len(got) != 0 {
		t.Fatalf("deny-all allowlist = %v", got)
	}
}

func TestDispatchSubcommandRoutesNousSetup(t *testing.T) {
	var stdout, stderr bytes.Buffer
	handled, code := dispatchSubcommand([]string{nousdeps.Subcommand}, &stdout, &stderr)
	if !handled {
		t.Fatal("nous-setup not handled")
	}
	if code != 2 || !strings.Contains(stderr.String(), "--campaign is required") {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
}
