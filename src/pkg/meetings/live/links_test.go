package live

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/betty"
)

func TestLinksChatText(t *testing.T) {
	got := linksChatText([]betty.Link{{Label: "Release plan", URL: "https://x.example/r"}, {Label: "hivecommons/hive#12", URL: "https://github.com/hivecommons/hive/issues/12"}, {Label: "empty"}})
	want := "Links for my answer:\nRelease plan: https://x.example/r\nhivecommons/hive#12: https://github.com/hivecommons/hive/issues/12"
	if got != want {
		t.Fatalf("got %q", got)
	}
	if linksChatText(nil) != "" {
		t.Fatal("no links, no line")
	}
}

type linkRetriever struct{}

func (linkRetriever) Name() string { return "links" }
func (linkRetriever) Retrieve(context.Context, string, []string, int) []betty.Snippet {
	return []betty.Snippet{{Source: "GitHub releases", Title: "Latest release of kubernetes/kubernetes", Text: "The latest published release is v1.37.2.",
		URL: "https://github.com/kubernetes/kubernetes/releases/tag/v1.37.2"}}
}

// A spoken answer is followed by its links in chat; the URL is never said.
func TestSpokenAnswerPostsLinksInChat(t *testing.T) {
	llm := funcLLM{fn: func(context.Context, string) (string, error) {
		return `{"answer":"The latest Kubernetes release is v1.37.2.","confidence":0.95,"sources":["1"]}`, nil
	}}
	h := bettyHarness(t, Policy{ChatPostsPerMinute: 20, VoiceOnAddress: true},
		meetings.Meeting{Platform: meetings.PlatformGoogleMeet, VoiceEnabled: true}, llm,
		func(_ *betty.Settings, d *betty.Deps) { d.Retrievers = []betty.Retriever{linkRetriever{}} })
	h.live()
	h.line(meetings.EventSegment, "Bo", "Betty, what is the latest Kubernetes release?", true)
	h.eventually("links posted", h.posted("https://github.com/kubernetes/kubernetes/releases/tag/v1.37.2"))
	posts, says := h.mt.got()
	for _, s := range says {
		if strings.Contains(s, "http") {
			t.Fatalf("a URL was spoken: %q", says)
		}
	}
	n := 0
	for _, p := range posts {
		if strings.HasPrefix(p, "Links for my answer:\nLatest release of kubernetes/kubernetes: https://") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want one links line, posts %q", posts)
	}
}
