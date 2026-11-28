package main

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

type gitSourceTestAlerts map[string]string

func (a gitSourceTestAlerts) AddSystemAlert(id, severity, message string) {
	a[id] = severity + ": " + message
}

func (a gitSourceTestAlerts) ClearSystemAlertsExcept(prefix string, keep map[string]struct{}) {
	for id := range a {
		if _, ok := keep[id]; strings.HasPrefix(id, prefix) && !ok {
			delete(a, id)
		}
	}
}

func TestGitSourceAlerts(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	sources := []knowledge.GitSourceInfo{
		{Name: "old", URL: "https://example.com/repo", Subpath: "old", FailureSince: now.Add(-time.Hour)},
		{Name: "recent", URL: "https://example.com/repo", Subpath: "recent", FailureSince: now.Add(-time.Minute)},
	}
	alerts := gitSourceTestAlerts{"unrelated": "keep"}
	reconcileGitSourceAlerts(alerts, sources, now)
	if len(alerts) != 2 {
		t.Fatalf("got alerts %v; want one stale source and unrelated alert", alerts)
	}
	for id, message := range alerts {
		if id != "unrelated" && (!strings.Contains(message, "old") || !strings.HasPrefix(message, "warning:")) {
			t.Errorf("unexpected alert: %s", message)
		}
	}
	sources[0].FailureSince = time.Time{}
	sources[1].FailureSince = now.Add(-time.Hour)
	reconcileGitSourceAlerts(alerts, sources, now)
	if len(alerts) != 2 {
		t.Fatalf("recovery should clear old alert and raise recent source: %v", alerts)
	}
	for id, message := range alerts {
		if id != "unrelated" && !strings.Contains(message, "recent") {
			t.Errorf("recovered source retained: %s", message)
		}
	}
	reconcileGitSourceAlerts(alerts, nil, now)
	if len(alerts) != 1 || alerts["unrelated"] != "keep" {
		t.Fatalf("disconnect should clear only source alerts: %v", alerts)
	}
}
