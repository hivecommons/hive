package github

import (
	"os"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// Phase 1 of kubestellar/hive#5953 removed this package's production
// dependency on pkg/config. Two values had to be duplicated to do that. This
// file is the only place pkg/config is still referenced here, and it exists to
// make the duplication safe: if either definition moves, the build of this
// test fails and the drift is caught at CI time rather than in behaviour.
//
// A test-only import does not reintroduce the coupling the issue is about —
// production code in this package no longer pulls in the application config
// surface, so importers of the client no longer do either.

func TestAutoMergeQueuedLabelMatchesConfigDefault(t *testing.T) {
	if AutoMergeQueuedLabel != config.DefaultAutoMergeLabel {
		t.Fatalf("AutoMergeQueuedLabel = %q, config.DefaultAutoMergeLabel = %q — "+
			"these name the same merger-queue label and must not drift",
			AutoMergeQueuedLabel, config.DefaultAutoMergeLabel)
	}
}

// config.IssueFilterConfig must keep satisfying IssueAdmitter, because every
// construction site passes it directly. If it stops, those sites break at the
// call rather than here, which is a much less obvious failure.
func TestIssueFilterConfigSatisfiesIssueAdmitter(t *testing.T) {
	var _ IssueAdmitter = config.IssueFilterConfig{}
}

// An unset filter must admit everything: that was the zero-value behaviour of
// the struct this interface replaced, and enumeration relies on it.
func TestUnsetIssueFilterAdmitsEverything(t *testing.T) {
	c := &Client{}
	if got := c.getIssueFilter(); got == nil {
		t.Fatal("getIssueFilter() returned nil; the enumeration path would panic")
	}
	if !c.getIssueFilter().Admits([]string{"anything"}) {
		t.Fatal("an unset filter rejected a label; unset must admit everything")
	}
	if !c.getIssueFilter().Admits(nil) {
		t.Fatal("an unset filter rejected an unlabelled issue")
	}
}

// The #1861 credential-divert switch is read independently by this package now.
// Compare the two readers BEHAVIOURALLY over the values an operator might
// plausibly set, including the near-misses: a duplicate that treated "TRUE" or
// "1" as enabled — or, worse, failed to treat "true" as enabled — would diverge
// from the configuration layer while looking correct.
func TestProxyInjectGHAuthMatchesConfig(t *testing.T) {
	for _, v := range []string{"", "true", " true ", "TRUE", "True", "1", "yes", "false", "  "} {
		t.Run("value="+v, func(t *testing.T) {
			t.Setenv(config.ProxyInjectGHAuthEnv, v)
			if got, want := proxyInjectGHAuth(), config.ProxyInjectGHAuth(); got != want {
				t.Fatalf("proxyInjectGHAuth()=%v but config.ProxyInjectGHAuth()=%v for %q — "+
					"the credential-divert switch must not diverge", got, want, v)
			}
		})
	}
	// And unset, which is the default posture.
	os.Unsetenv(config.ProxyInjectGHAuthEnv)
	if proxyInjectGHAuth() != config.ProxyInjectGHAuth() {
		t.Fatal("readers diverge when the env var is unset")
	}
	if proxyInjectGHAuth() {
		t.Fatal("divert must default to OFF when unset")
	}
}

// The env var name itself must match, or the local reader silently watches a
// variable nobody sets.
func TestProxyInjectGHAuthEnvNameMatchesConfig(t *testing.T) {
	if proxyInjectGHAuthEnv != config.ProxyInjectGHAuthEnv {
		t.Fatalf("env name %q != config %q", proxyInjectGHAuthEnv, config.ProxyInjectGHAuthEnv)
	}
}
