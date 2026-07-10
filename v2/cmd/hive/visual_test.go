package main

import "testing"

func TestParseRepairCommandsUsesStructuredArguments(t *testing.T) {
	commands, err := parseRepairCommands([]string{`["npm","run","typecheck"]`, `["npm","test","--","--runInBand"]`})
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 || commands[0].Name != "npm" || len(commands[0].Args) != 2 || commands[0].Args[1] != "typecheck" {
		t.Fatalf("unexpected commands: %+v", commands)
	}
}

func TestParseRepairCommandsRejectsShellString(t *testing.T) {
	if _, err := parseRepairCommands([]string{`npm test && curl example.test`}); err == nil {
		t.Fatal("expected unstructured shell command to be rejected")
	}
}
