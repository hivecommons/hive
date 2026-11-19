package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
)

func topicStoreOpener(t *testing.T) func() *beads.Store {
	t.Helper()
	dir := t.TempDir()
	return func() *beads.Store {
		s, err := beads.NewStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
}

func runTopicT(open func() *beads.Store, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := runTopic(args, open, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestTopicClaimGetListRelease(t *testing.T) {
	open := topicStoreOpener(t)
	t.Setenv("HIVE_AGENT", "deps")

	code, out, errOut := runTopicT(open, "claim", "deps-bump", "--repo", "acme/widget", "--pr", "12", "--branch", "deps/a")
	if code != topicExitOK {
		t.Fatalf("claim exit %d: %s", code, errOut)
	}
	var claim beads.TopicClaim
	if err := json.Unmarshal([]byte(out), &claim); err != nil || claim.PR != 12 || claim.Actor != "deps" {
		t.Fatalf("claim output %q (%v)", out, err)
	}

	code, _, errOut = runTopicT(open, "claim", "deps-bump", "--repo", "acme/widget", "--pr", "13")
	if code != topicExitRefused || !strings.Contains(errOut, "refused") || !strings.Contains(errOut, "acme/widget#12") {
		t.Fatalf("second claim exit %d: %s", code, errOut)
	}

	if code, out, _ = runTopicT(open, "get", "deps-bump"); code != topicExitOK || !strings.Contains(out, "deps-bump → acme/widget#12 (branch deps/a)") {
		t.Fatalf("get exit %d: %q", code, out)
	}
	if code, out, _ = runTopicT(open, "get", "deps-bump", "--json"); code != topicExitOK || !strings.Contains(out, `"pr": 12`) {
		t.Fatalf("get --json exit %d: %q", code, out)
	}
	if code, out, _ = runTopicT(open, "list"); code != topicExitOK || !strings.Contains(out, "deps-bump → acme/widget#12") {
		t.Fatalf("list exit %d: %q", code, out)
	}
	if code, out, _ = runTopicT(open, "list", "--json"); code != topicExitOK || !strings.Contains(out, `"key": "deps-bump"`) {
		t.Fatalf("list --json exit %d: %q", code, out)
	}

	if code, out, _ = runTopicT(open, "release", "deps-bump"); code != topicExitOK || !strings.Contains(out, "Released") {
		t.Fatalf("release exit %d: %q", code, out)
	}
	if code, out, _ = runTopicT(open, "release", "deps-bump"); code != topicExitOK || !strings.Contains(out, "No open claim") {
		t.Fatalf("second release exit %d: %q", code, out)
	}
	if code, _, _ = runTopicT(open, "get", "deps-bump"); code != topicExitRefused {
		t.Fatalf("get after release exit %d", code)
	}
	if code, out, _ = runTopicT(open, "list"); code != topicExitOK || !strings.Contains(out, "No open topic claims.") {
		t.Fatalf("empty list exit %d: %q", code, out)
	}
	if code, out, _ = runTopicT(open, "list", "--json"); code != topicExitOK || strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty list --json exit %d: %q", code, out)
	}
}

func TestTopicUsageErrors(t *testing.T) {
	open := topicStoreOpener(t)
	for name, args := range map[string][]string{
		"no subcommand":   {},
		"unknown":         {"frob"},
		"missing key":     {"claim"},
		"flag as key":     {"get", "--json"},
		"bad claim flag":  {"claim", "k", "--nope"},
		"claim no pr":     {"claim", "k", "--repo", "a/b"},
		"bad get flag":    {"get", "k", "--nope"},
		"bad list flag":   {"list", "--nope"},
		"bad release key": {"release", "a b"},
	} {
		if code, _, _ := runTopicT(open, args...); code != topicExitError {
			t.Errorf("%s: exit %d, want %d", name, code, topicExitError)
		}
	}
}

func TestMainDispatchTopic(t *testing.T) {
	t.Setenv("BD_DIR", t.TempDir())
	out := runMain(t, "topic", "list")
	if !strings.Contains(out, "No open topic claims.") {
		t.Fatalf("main(topic list) = %q", out)
	}
}
