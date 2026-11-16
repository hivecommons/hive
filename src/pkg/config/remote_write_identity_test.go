package config

import "testing"

func TestRemoteWriteIdentity(t *testing.T) {
	cases := []struct {
		in        string
		valid     bool
		effective string
	}{
		{"", true, RemoteWriteIdentityBot},
		{"bot", true, RemoteWriteIdentityBot},
		{" Bot ", true, RemoteWriteIdentityBot},
		{"bot_only", true, RemoteWriteIdentityBotOnly},
		{"bot-only", true, RemoteWriteIdentityBotOnly},
		{"owner", true, RemoteWriteIdentityOwner},
		{"OWNER", true, RemoteWriteIdentityOwner},
		// An unknown value is refused at load and, if it ever slipped
		// through, reads as the identity that never uses the owner's name.
		{"me", false, RemoteWriteIdentityBotOnly},
		{"app", false, RemoteWriteIdentityBotOnly},
	}
	for _, tc := range cases {
		err := ValidateRemoteWriteIdentity(tc.in)
		if (err == nil) != tc.valid {
			t.Errorf("ValidateRemoteWriteIdentity(%q) err=%v, want valid=%v", tc.in, err, tc.valid)
		}
		if got := (ProjectConfig{RemoteWriteIdentity: tc.in}).EffectiveRemoteWriteIdentity(); got != tc.effective {
			t.Errorf("EffectiveRemoteWriteIdentity(%q) = %q, want %q", tc.in, got, tc.effective)
		}
	}
	if len(RemoteWriteIdentities) != 3 || RemoteWriteIdentities[0] != RemoteWriteIdentityBot {
		t.Errorf("vocabulary must lead with the default: %v", RemoteWriteIdentities)
	}
}
