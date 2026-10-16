package config

import (
	"strings"
	"testing"
)

func TestValidateSnapshotURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "empty is unset", in: "", want: ""},
		{name: "whitespace is unset", in: "   ", want: ""},
		{name: "https origin", in: "https://snap.example.com", want: "https://snap.example.com"},
		{name: "http origin with port", in: "http://snap.internal:8080", want: "http://snap.internal:8080"},
		{name: "https origin trailing slash trimmed", in: "https://snap.example.com/", want: "https://snap.example.com"},
		{name: "https path allowed", in: "https://hive.example.com/snapshot", want: "https://hive.example.com/snapshot"},
		{name: "https path trailing slash trimmed", in: "https://hive.example.com/snapshot/", want: "https://hive.example.com/snapshot"},
		{name: "surrounding whitespace trimmed", in: "  https://snap.example.com/snapshot/  ", want: "https://snap.example.com/snapshot"},
		{name: "uppercase scheme lowercased", in: "HTTPS://snap.example.com/snapshot", want: "https://snap.example.com/snapshot"},
		{name: "bare trailing hash dropped", in: "https://snap.example.com/snapshot#", want: "https://snap.example.com/snapshot"},
		{name: "bare trailing hash on origin dropped", in: "https://snap.example.com#", want: "https://snap.example.com"},
		{name: "no scheme", in: "snap.example.com", wantErr: "absolute http:// or https://"},
		{name: "wrong scheme", in: "ftp://snap.example.com", wantErr: "absolute http:// or https://"},
		{name: "missing host", in: "https:///snapshot", wantErr: "missing host"},
		{name: "query rejected", in: "https://snap.example.com/snapshot?view=1", wantErr: "query and fragment are not allowed"},
		{name: "fragment rejected", in: "https://snap.example.com/snapshot#top", wantErr: "query and fragment are not allowed"},
		{name: "credentials rejected", in: "https://u:p@snap.example.com/snapshot", wantErr: "credentials"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateSnapshotURL(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ValidateSnapshotURL(%q) = %q, want error containing %q", tc.in, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				if !strings.Contains(err.Error(), "hub.snapshot_url") {
					t.Fatalf("error = %q, should name the field", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoad_HubSnapshotURL(t *testing.T) {
	base := minimalValidYAML("acme", "ghp_tok")

	path := writeTempConfig(t, base+"\nhub:\n  snapshot_url: https://snap.example.com/snapshot/\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() with valid snapshot_url: %v", err)
	}
	if cfg.Hub.SnapshotURL != "https://snap.example.com/snapshot" {
		t.Fatalf("Hub.SnapshotURL = %q, want trailing slash trimmed", cfg.Hub.SnapshotURL)
	}

	path = writeTempConfig(t, base+"\nhub:\n  snapshot_url: snap.example.com/snapshot\n")
	_, err = Load(path)
	if err == nil {
		t.Fatal("Load() expected error for snapshot_url without scheme, got nil")
	}
	if !strings.Contains(err.Error(), "hub.snapshot_url") {
		t.Fatalf("error = %q, should name hub.snapshot_url", err.Error())
	}
}
