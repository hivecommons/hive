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
		{name: "bare query rejected", in: "https://snap.example.com/snapshot?", wantErr: "query and fragment are not allowed"},
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

func TestLoad_HubSnapshotURLLegacyWarning(t *testing.T) {
	legacyURL := "https://snap.example.com/snapshot?token=private-snapshot-token#top"
	path := writeTempConfig(t, minimalValidYAML("acme", "ghp_tok")+"\nhub:\n  snapshot_url: "+legacyURL+"\n")
	logs := captureLog(t, func() {
		if _, err := Load(path); err != nil {
			t.Fatalf("Load() with legacy snapshot_url: %v", err)
		}
	})
	if !strings.Contains(logs, "warning: hub.snapshot_url contains a legacy query or fragment") {
		t.Fatalf("missing legacy URL warning in logs: %s", logs)
	}
	if strings.Contains(logs, "private-snapshot-token") {
		t.Fatal("legacy URL warning leaked query credentials")
	}
}

// Validation errors name the field and host but must never echo query
// strings or userinfo, which legacy URLs may use to carry secrets.
func TestValidateSnapshotURL_ErrorsRedactSecrets(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{name: "query on rejected scheme", in: "ftp://snap.example.com/snapshot?token=private-snapshot-token"},
		{name: "query with credentials", in: "https://user:private-snapshot-token@snap.example.com/snapshot?token=private-snapshot-token"},
		{name: "query rejected by dashboard", in: "https://snap.example.com/snapshot?token=private-snapshot-token#private-fragment"},
		{name: "query on unparseable url", in: "https://snap.example.com:bad/snapshot?token=private-snapshot-token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateSnapshotURL(tc.in)
			if err == nil {
				t.Fatalf("ValidateSnapshotURL(%q) expected error", tc.in)
			}
			msg := err.Error()
			if !strings.Contains(msg, "hub.snapshot_url") || !strings.Contains(msg, "snap.example.com") {
				t.Fatalf("error = %q, should name the field and host", msg)
			}
			if strings.Contains(msg, "private-") {
				t.Fatalf("error = %q, leaked a secret-bearing URL component", msg)
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

	for _, legacyURL := range []string{
		"https://snap.example.com/snapshot/?view=1",
		"https://snap.example.com/snapshot/#top",
		"https://snap.example.com/snapshot/?view=1#top",
		"http://snap.internal:8080/?",
		"https://snap.example.com/snap%2Fshot/?view=a%2Fb#top",
	} {
		t.Run(legacyURL, func(t *testing.T) {
			path := writeTempConfig(t, base+"\nhub:\n  snapshot_url: "+legacyURL+"\n")
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load() must accept legacy query/fragment: %v", err)
			}
			if cfg.Hub.SnapshotURL != legacyURL {
				t.Fatalf("Hub.SnapshotURL = %q, want legacy URL preserved as %q", cfg.Hub.SnapshotURL, legacyURL)
			}
		})
	}

	for _, invalidURL := range []string{
		"ftp://snap.example.com/snapshot?view=1",
		"https:///snapshot#top",
		"https://u:p@snap.example.com/snapshot?view=1",
		"https://snap.example.com/%zz?view=1",
	} {
		t.Run(invalidURL, func(t *testing.T) {
			path := writeTempConfig(t, base+"\nhub:\n  snapshot_url: "+invalidURL+"\n")
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "hub.snapshot_url") {
				t.Fatalf("Load() error = %v, want snapshot URL validation error", err)
			}
		})
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
