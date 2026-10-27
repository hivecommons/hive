package gitidentity

import "testing"

func TestAgentIdentityUsesDefaultDomain(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "")

	name, email, ok := AgentIdentity("quality-agent")
	if !ok {
		t.Fatal("AgentIdentity returned ok=false for a safe agent name")
	}
	if name != "quality-agent" {
		t.Fatalf("name = %q, want quality-agent", name)
	}
	if email != "quality-agent@"+DefaultBotEmailDomain {
		t.Fatalf("email = %q, want default domain", email)
	}
}

func TestAgentIdentityHonorsValidDomain(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "bots.example")

	_, email, ok := AgentIdentity("release_5")
	if !ok {
		t.Fatal("AgentIdentity returned ok=false for underscore-safe agent name")
	}
	if email != "release_5@bots.example" {
		t.Fatalf("email = %q, want release_5@bots.example", email)
	}
}

func TestAgentIdentityRejectsUnsafeAgentName(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "bots.example")

	if name, email, ok := AgentIdentity("bad/name"); ok || name != "" || email != "" {
		t.Fatalf("AgentIdentity unsafe name = (%q, %q, %v), want empty false", name, email, ok)
	}
}

func TestAgentIdentityFallsBackFromUnsafeDomain(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "bad/domain")

	_, email, ok := AgentIdentity("worker")
	if !ok {
		t.Fatal("AgentIdentity returned ok=false for safe agent name")
	}
	if email != "worker@"+DefaultBotEmailDomain {
		t.Fatalf("email = %q, want default domain", email)
	}
}

func TestAppLinkedAgentIdentityUsesBotNoreply(t *testing.T) {
	// 280983584 is the bot USER id GitHub links noreply addresses by; the App
	// id (3568013) would produce an address GitHub never links.
	name, email, ok := AppLinkedAgentIdentity("scanner", 280983584, "hivecommons-hive[bot]")
	if !ok {
		t.Fatal("AppLinkedAgentIdentity returned ok=false")
	}
	if name != "scanner" {
		t.Fatalf("name = %q, want scanner", name)
	}
	if want := "280983584+hivecommons-hive[bot]@users.noreply.github.com"; email != want {
		t.Fatalf("email = %q, want %q", email, want)
	}
}

func TestAppLinkedAgentIdentityRejectsIncompleteInputs(t *testing.T) {
	for _, tc := range []struct {
		agent     string
		botUserID int64
		bot       string
	}{
		{agent: "bad/name", botUserID: 1, bot: "hive[bot]"},
		{agent: "scanner", botUserID: 0, bot: "hive[bot]"},
		{agent: "scanner", botUserID: 1, bot: ""},
		{agent: "scanner", botUserID: 1, bot: "bad bot"},
	} {
		if name, email, ok := AppLinkedAgentIdentity(tc.agent, tc.botUserID, tc.bot); ok || name != "" || email != "" {
			t.Fatalf("AppLinkedAgentIdentity(%q,%d,%q) = (%q,%q,%v), want empty false", tc.agent, tc.botUserID, tc.bot, name, email, ok)
		}
	}
}
