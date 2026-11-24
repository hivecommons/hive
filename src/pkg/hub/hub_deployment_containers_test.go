package hub

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

// #19344: a stray "hive-hub" container ran beside the real "hub" container.
func TestUnexpectedHubContainers(t *testing.T) {
	cases := []struct {
		name       string
		containers []string
		want       []string
	}{
		{"only hub", []string{"hub"}, nil},
		{"stray hive-hub", []string{"hive-hub", "hub"}, []string{"hive-hub"}},
		{"two strays", []string{"hub", "sidecar", "hive-hub"}, []string{"sidecar", "hive-hub"}},
		{"empty", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unexpectedHubContainers(tc.containers); !slices.Equal(got, tc.want) {
				t.Fatalf("unexpectedHubContainers(%v) = %v, want %v", tc.containers, got, tc.want)
			}
		})
	}
}

func TestWarnUnexpectedHubContainers(t *testing.T) {
	cases := []struct {
		name     string
		names    string
		wantWarn bool
	}{
		{"stray container warns", "hive-hub hub", true},
		{"only hub is quiet", "hub", false},
		{"unreadable is quiet", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeFakeKubectl(t, "#!/bin/sh\ncase \"$*\" in *containers*) echo \""+tc.names+"\";; esac\nexit 0\n")
			var buf bytes.Buffer
			s := bulkTestHub(t)
			s.logger = slog.New(slog.NewTextHandler(&buf, nil))
			s.warnUnexpectedHubContainers()
			got := strings.Contains(buf.String(), "unexpected containers")
			if got != tc.wantWarn {
				t.Fatalf("warned = %v, want %v; log:\n%s", got, tc.wantWarn, buf.String())
			}
			if tc.wantWarn && !strings.Contains(buf.String(), "unexpected=hive-hub") {
				t.Fatalf("warning must name the stray container; log:\n%s", buf.String())
			}
		})
	}
}
