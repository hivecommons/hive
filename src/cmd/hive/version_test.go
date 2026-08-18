package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCommandReportsEmbeddedIntegratedIdentity(t *testing.T) {
	oldVersion, oldHash := integratedVersion, gitHash
	integratedVersion = "v0.4.1-integrated.11"
	gitHash = strings.Repeat("a", 40)
	t.Cleanup(func() { integratedVersion, gitHash = oldVersion, oldHash })

	for _, argument := range []string{"--version", "version"} {
		var stdout, stderr bytes.Buffer
		if code := runVersionCommand([]string{argument}, &stdout, &stderr); code != 0 {
			t.Fatalf("hive %s exit = %d, stderr=%q", argument, code, stderr.String())
		}
		want := "Hive v0.4.1-integrated.11\ncommit: " + strings.Repeat("a", 40) + "\n"
		if stdout.String() != want || stderr.Len() != 0 {
			t.Fatalf("hive %s output = %q, stderr=%q, want %q", argument, stdout.String(), stderr.String(), want)
		}
	}
}

func TestVersionCommandRejectsExtraArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runVersionCommand([]string{"--version", "extra"}, &stdout, &stderr); code != 2 {
		t.Fatalf("extra version argument exit = %d, want 2", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "usage: hive --version") {
		t.Fatalf("extra version argument output = %q, stderr=%q", stdout.String(), stderr.String())
	}
}
