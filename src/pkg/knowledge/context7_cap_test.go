package knowledge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// #12124: Context7 bodies are bounded to whole snippets with a visible marker.

func context7Snippet(i int, topic string, codeLen int) string {
	return fmt.Sprintf("TITLE: Snippet %d %s\nDESCRIPTION: about %s\nSOURCE: https://example.com/%d\n\nLANGUAGE: go\nCODE:\n```\n%s\n```",
		i, topic, topic, i, strings.Repeat("c", codeLen))
}

func context7Body(snippets ...string) string {
	return strings.Join(snippets, "\n\n"+context7SnippetSeparator+"\n\n")
}

func TestCapContext7Content_UnderCapKeepsEverythingInOrder(t *testing.T) {
	body := context7Body(context7Snippet(1, "routing", 50), context7Snippet(2, "images", 50), context7Snippet(3, "routing", 50))
	out, omitted := CapContext7Content(body, "images", DefaultContext7Caps)
	if omitted.Any() {
		t.Fatalf("nothing should be omitted: %+v", omitted)
	}
	if strings.Contains(out, Context7TruncatedMarker) {
		t.Errorf("marker rendered without truncation")
	}
	// Relevance only decides what to drop; kept snippets stay in source order.
	i1, i2, i3 := strings.Index(out, "Snippet 1"), strings.Index(out, "Snippet 2"), strings.Index(out, "Snippet 3")
	if !(i1 >= 0 && i1 < i2 && i2 < i3) {
		t.Errorf("document order not preserved: %d %d %d\n%s", i1, i2, i3, out)
	}
	if strings.Count(out, context7SnippetSeparator) != 2 {
		t.Errorf("separators should be re-emitted between kept snippets:\n%s", out)
	}
}

func TestCapContext7Content_TotalCapDropsWholeSnippetsPreferringQuery(t *testing.T) {
	var snippets []string
	for i := 1; i <= 10; i++ {
		topic := "filler"
		if i == 7 || i == 9 {
			topic = "routing"
		}
		snippets = append(snippets, context7Snippet(i, topic, 300))
	}
	caps := Context7Caps{MaxTotalBytes: 2048}
	out, omitted := CapContext7Content(context7Body(snippets...), "routing", caps)
	if !omitted.Any() {
		t.Fatal("expected truncation")
	}
	if len(out) > caps.MaxTotalBytes {
		t.Errorf("output %d bytes exceeds cap %d", len(out), caps.MaxTotalBytes)
	}
	// Query-matching snippets survive even though they come late in the body.
	for _, want := range []string{"Snippet 7 routing", "Snippet 9 routing"} {
		if !strings.Contains(out, want) {
			t.Errorf("relevant %q should have been kept:\n%s", want, out)
		}
	}
	// Whole-snippet drop: every kept code block is intact.
	kept := strings.Count(out, "TITLE: Snippet")
	if kept+omitted.Snippets != 10 {
		t.Errorf("kept %d + omitted %d != 10", kept, omitted.Snippets)
	}
	if strings.Count(out, strings.Repeat("c", 300)) != kept {
		t.Errorf("a kept snippet was clipped:\n%s", out)
	}
	want := Context7TruncatedMarker + " " + strconv.Itoa(omitted.Snippets) + " snippets / " + strconv.Itoa(omitted.Bytes) + " bytes omitted (size cap)\n"
	if !strings.HasSuffix(out, want) {
		t.Errorf("marker should end the content, want %q, tail %q", want, out[len(out)-100:])
	}
}

func TestCapContext7Content_SnippetAndCountCaps(t *testing.T) {
	body := context7Body(context7Snippet(1, "a", 5000), context7Snippet(2, "b", 10), context7Snippet(3, "c", 10), context7Snippet(4, "d", 10))
	caps := Context7Caps{MaxSnippetBytes: 1024, MaxSnippets: 2}
	out, omitted := CapContext7Content(body, "", caps)
	if strings.Contains(out, "Snippet 1 a") {
		t.Errorf("oversized snippet must be dropped whole, not clipped")
	}
	if omitted.Snippets != 2 {
		t.Errorf("expected the oversized snippet plus one past the count cap omitted, got %+v", omitted)
	}
	if !strings.Contains(out, "Snippet 2 b") || !strings.Contains(out, "Snippet 3 c") {
		t.Errorf("smaller later snippets should be kept up to the count cap:\n%s", out)
	}
	if strings.Contains(out, "Snippet 4 d") {
		t.Errorf("count cap not honoured:\n%s", out)
	}
}

func TestCapContext7Content_ProseFallsBackToParagraphs(t *testing.T) {
	body := "Para one about routing.\n\nPara two.\n\n" + strings.Repeat("z", 3000) + "\n\nPara four about routing."
	out, omitted := CapContext7Content(body, "routing", Context7Caps{MaxTotalBytes: 1024})
	if omitted.Snippets != 1 || strings.Contains(out, "zzzz") {
		t.Errorf("oversized paragraph should be the one dropped: %+v\n%s", omitted, out)
	}
	if !strings.Contains(out, "Para one") || !strings.Contains(out, "Para four") {
		t.Errorf("paragraphs that fit must be kept:\n%s", out)
	}
	if out == "" || !strings.Contains(out, Context7TruncatedMarker) {
		t.Errorf("marker missing:\n%s", out)
	}
}

func TestCapContext7Content_Empty(t *testing.T) {
	out, omitted := CapContext7Content("  \n", "q", DefaultContext7Caps)
	if out != "" || omitted.Any() {
		t.Errorf("empty body -> %q %+v", out, omitted)
	}
}

func TestContext7CapsFromEnv(t *testing.T) {
	t.Setenv(Context7EnvMaxSnippets, "3")
	t.Setenv(Context7EnvMaxSnippetBytes, "not-a-number")
	t.Setenv(Context7EnvMaxTotalBytes, "100") // below floor
	caps := Context7CapsFromEnv()
	if caps.MaxSnippets != 3 {
		t.Errorf("MaxSnippets = %d", caps.MaxSnippets)
	}
	if caps.MaxSnippetBytes != Context7DefaultMaxSnippetBytes {
		t.Errorf("bad override should fall back to default, got %d", caps.MaxSnippetBytes)
	}
	if caps.MaxTotalBytes != context7MinTotalBytes {
		t.Errorf("tiny total should be raised to floor %d, got %d", context7MinTotalBytes, caps.MaxTotalBytes)
	}
}

func TestFetchContext7Docs_CapsBodyAndReportsOmitted(t *testing.T) {
	var snippets []string
	for i := 1; i <= 30; i++ {
		snippets = append(snippets, context7Snippet(i, "topic", 400))
	}
	body := context7Body(snippets...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	useContext7TestServer(t, srv.URL)
	t.Setenv(Context7EnvMaxTotalBytes, "4096")

	got, err := FetchContext7Docs(context.Background(), "/org/lib", "topic", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Omitted.Any() {
		t.Fatal("expected snippets to be omitted under a 4 KiB cap")
	}
	if len(got.Content) > 4096 {
		t.Errorf("content %d bytes exceeds cap", len(got.Content))
	}
	if !strings.Contains(got.Content, Context7TruncatedMarker) {
		t.Error("content should carry the truncation marker")
	}
}
