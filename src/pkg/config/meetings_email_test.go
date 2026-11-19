package config

import "testing"

func TestMeetingsEmailToggles(t *testing.T) {
	t.Setenv(MeetingsEmailInvitesEnv, "")
	t.Setenv(MeetingsEmailFilesEnv, "")
	var e MeetingsEmailConfig
	if !e.InvitesOn() || !e.FilesOn() {
		t.Fatal("both default on")
	}
	off := false
	e = MeetingsEmailConfig{InvitesEnabled: &off, FilesEnabled: &off}
	if e.InvitesOn() || e.FilesOn() {
		t.Fatal("config off")
	}
	t.Setenv(MeetingsEmailInvitesEnv, "on")
	t.Setenv(MeetingsEmailFilesEnv, "yes")
	if !e.InvitesOn() || !e.FilesOn() {
		t.Fatal("env wins over config")
	}
	t.Setenv(MeetingsEmailInvitesEnv, "0")
	t.Setenv(MeetingsEmailFilesEnv, "false")
	if (MeetingsEmailConfig{}).InvitesOn() || (MeetingsEmailConfig{}).FilesOn() {
		t.Fatal("env off wins over the default")
	}
}
