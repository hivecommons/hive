package hub

import "testing"

func TestSpokeAppAuthFailing(t *testing.T) {
	tests := []struct {
		name    string
		payload *HeartbeatPayload
		want    bool
	}{
		{
			name:    "nil payload returns false",
			payload: nil,
			want:    false,
		},
		{
			name:    "app not required returns false",
			payload: &HeartbeatPayload{GitHubAppRequired: false, GitHubAppState: "not-installed"},
			want:    false,
		},
		{
			name:    "app required, state not-installed returns true",
			payload: &HeartbeatPayload{GitHubAppRequired: true, GitHubAppState: "not-installed"},
			want:    true,
		},
		{
			name:    "app required, state wrong-installation returns true",
			payload: &HeartbeatPayload{GitHubAppRequired: true, GitHubAppState: "wrong-installation"},
			want:    true,
		},
		{
			name:    "app required, state key-invalid returns true",
			payload: &HeartbeatPayload{GitHubAppRequired: true, GitHubAppState: "key-invalid"},
			want:    true,
		},
		{
			name:    "app required, state key-missing returns true",
			payload: &HeartbeatPayload{GitHubAppRequired: true, GitHubAppState: "key-missing"},
			want:    true,
		},
		{
			name:    "app required, state with leading whitespace returns true",
			payload: &HeartbeatPayload{GitHubAppRequired: true, GitHubAppState: "  not-installed  "},
			want:    true,
		},
		{
			name:    "app required, state healthy returns false",
			payload: &HeartbeatPayload{GitHubAppRequired: true, GitHubAppState: "healthy"},
			want:    false,
		},
		{
			name:    "app required, empty state returns false",
			payload: &HeartbeatPayload{GitHubAppRequired: true, GitHubAppState: ""},
			want:    false,
		},
		{
			name:    "app required, state installed returns false",
			payload: &HeartbeatPayload{GitHubAppRequired: true, GitHubAppState: "installed"},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := spokeAppAuthFailing(tt.payload)
			if got != tt.want {
				t.Errorf("spokeAppAuthFailing() = %v, want %v", got, tt.want)
			}
		})
	}
}
