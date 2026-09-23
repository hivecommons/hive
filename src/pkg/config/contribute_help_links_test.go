package config

import (
	"strings"
	"testing"
)

func TestNormalizeContributeHelpLinks(t *testing.T) {
	links, err := NormalizeContributeHelpLinks([]ContributeHelpLink{
		{Label: "  Chat\nwith <b>contributors</b>  ", URL: "https://discord.gg/hive "},
		{Label: strings.Repeat("x", ContributeHelpLinkLabelMaxRunes+5), URL: "http://example.test/help"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if links[0].Label != "Chat with <b>contributors</b>" {
		t.Fatalf("label normalized as plain text = %q", links[0].Label)
	}
	if got := len([]rune(links[1].Label)); got != ContributeHelpLinkLabelMaxRunes {
		t.Fatalf("label length = %d, want %d", got, ContributeHelpLinkLabelMaxRunes)
	}
}

func TestNormalizeContributeHelpLinksRejectsUnsafeURLs(t *testing.T) {
	for _, raw := range []string{"javascript:alert(1)", "ftp://example.test/help", "/relative"} {
		if _, err := NormalizeContributeHelpLinks([]ContributeHelpLink{{Label: "Help", URL: raw}}); err == nil {
			t.Fatalf("NormalizeContributeHelpLinks accepted %q", raw)
		}
	}
}

func TestContributeHelpLinksDefault(t *testing.T) {
	links := ContributeHelpLinksOrDefault(nil)
	if len(links) < 2 {
		t.Fatalf("default links = %#v, want docs and issue tracker", links)
	}
	if !strings.Contains(links[0].URL, "src/docs/contributor-relay.md") {
		t.Fatalf("default docs link = %#v", links[0])
	}
}
