package config

import (
	"path"
	"regexp"
	"testing"

	"gopkg.in/yaml.v3"
)

// supervisorRestartWording matches pack text that says the supervisor restarts
// or self-heals agents. The supervisor never restarts agents at any ACMM level
// (hivecommons/hive#19945): it reports stalled agents in advisory beads.
var supervisorRestartWording = regexp.MustCompile(`(?i)restart|self-heal`)

func TestACMMPackSupervisorNeverClaimsRestarts(t *testing.T) {
	entries, err := packsFS.ReadDir("packs")
	if err != nil {
		t.Fatalf("read packs: %v", err)
	}
	checked := 0
	for _, entry := range entries {
		if path.Ext(entry.Name()) != ".yaml" {
			continue
		}
		data, err := packsFS.ReadFile(path.Join("packs", entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		var pack ACMMPack
		if err := yaml.Unmarshal(data, &pack); err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, agent := range pack.Agents {
			if agent.Role != "supervisor" {
				continue
			}
			checked++
			for field, text := range map[string]string{
				"description":   agent.Description,
				"interactions":  agent.Interactions,
				"knowledge_use": agent.KnowledgeUse,
			} {
				if m := supervisorRestartWording.FindString(text); m != "" {
					t.Errorf("%s supervisor %s says %q: the supervisor never restarts agents", entry.Name(), field, m)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no supervisor agent found in any embedded pack")
	}
}
