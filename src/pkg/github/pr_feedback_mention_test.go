package github

import "testing"

// cncf/techdocs#424: the reviewer typed "@hivecommons-hive" (no "[bot]") on
// a hive whose ai_author is empty, and the comment was never routed.
func TestAddressesHive_BareAppSlug(t *testing.T) {
	c := testClient(t, "http://127.0.0.1:1")
	c.SetAppBotLogin("hivecommons-hive[bot]")
	c.SetHiveIdentity(HiveIdentity{AppLogin: "hivecommons-hive[bot]"})
	cases := []struct {
		body string
		want bool
	}{
		{"Reverting to draft. @hivecommons-hive, please do another revision based on new review feedback.", true},
		{"@hivecommons-hive[bot] please revise", true},
		{"@HiveCommons-Hive please revise", true},
		{"thanks @hivecommons-hive.", true},
		{"(@hivecommons-hive)", true},
		{"@hivecommons-hive", true},
		{"/hive revisit this", true},
		{"@hivecommons-hive-foo please look", false},
		{"@hivecommons-hive_x please look", false},
		{"mail bob@hivecommons-hive about it", false},
		{"run `@hivecommons-hive retry` in chat", false},
		{"```\n@hivecommons-hive do it\n```", false},
		{"> @hivecommons-hive please revise\n\nagreed with the above", false},
		{"thanks all", false},
		{"@hivecommons please look", false},
	}
	for _, tc := range cases {
		if got := c.addressesHive(tc.body); got != tc.want {
			t.Errorf("addressesHive(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// The AI author account is matched as a whole mention too: a longer login
// that merely starts with it does not address the hive.
func TestAddressesHive_AIAuthorWholeMention(t *testing.T) {
	c := humanCommentTestClient(t, "http://127.0.0.1:1")
	if !c.addressesHive("@hive-ai please recheck") {
		t.Fatal("ai_author mention not matched")
	}
	if c.addressesHive("@hive-aide please recheck") {
		t.Fatal("longer login matched as ai_author")
	}
	if !c.addressesHive("@hive please recheck") {
		t.Fatal("bare App slug not matched")
	}
}

func TestHiveMentionLogins_ExpandsBotSlug(t *testing.T) {
	got := hiveMentionLogins("Hive[bot]", "hive[bot]", "", "  ai  ")
	want := []string{"hive[bot]", "hive", "ai"}
	if len(got) != len(want) {
		t.Fatalf("logins = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("logins = %v, want %v", got, want)
		}
	}
}
