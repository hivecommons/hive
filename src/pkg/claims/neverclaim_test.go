package claims

import "testing"

func TestNeverClaimLabel(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		never  []string
		want   string
	}{
		{"default matches Epic case-insensitively", []string{"help wanted", "Epic"}, nil, "Epic"},
		{"default matches lowercase epic with padding", []string{" epic "}, nil, "epic"},
		{"no umbrella label", []string{"bug", "kind/epic-ish"}, nil, ""},
		{"empty labels", nil, nil, ""},
		{"configured set replaces the default", []string{"Epic", "tracking"}, []string{"tracking"}, "tracking"},
		{"configured set without epic lets an epic through", []string{"Epic"}, []string{"tracking"}, ""},
		{"empty configured set disables the gate", []string{"Epic"}, []string{}, ""},
		{"blank configured entries are ignored", []string{"Epic", ""}, []string{" ", "EPIC"}, "Epic"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NeverClaimLabel(tc.labels, tc.never); got != tc.want {
				t.Fatalf("NeverClaimLabel(%v, %v) = %q, want %q", tc.labels, tc.never, got, tc.want)
			}
		})
	}
}

func TestNeverClaimReason(t *testing.T) {
	rfc := "<!-- spek:publish name=n artifact_id=a target=github -->"
	child := "<!-- spek:publish name=n artifact_id=a target=github task=t1 -->"
	cases := []struct {
		name   string
		labels []string
		never  []string
		body   string
		want   string
	}{
		{"label wins over marker", []string{"Epic"}, nil, rfc, "Epic"},
		{"spek rfc without label", []string{"enhancement"}, nil, rfc, SpekUmbrellaMarker},
		{"spek rfc with gate disabled still umbrella", nil, []string{}, rfc, SpekUmbrellaMarker},
		{"spek child is work", nil, nil, child, ""},
		{"plain body", []string{"bug"}, nil, "no marker here", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NeverClaimReason(tc.labels, tc.never, tc.body); got != tc.want {
				t.Fatalf("NeverClaimReason = %q, want %q", got, tc.want)
			}
		})
	}
}
