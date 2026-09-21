package github

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The exact body shape from hivecommons/hive#8067: the reviewer quoted a line
// whose credential-shaped literal had been masked, then reported the mask as
// the defect.
const maskedFindingBody = "The format string never interpolates the token:\n\n" +
	"```bash\n" +
	"stdin_config=\"$(printf 'header = \"Authorization: ******\"\\n' \"$LLMMAN_TOKEN_VALUE\")\"\n" +
	"```\n\n" +
	"It has no `%s` (and no `Bearer` scheme), so the token is never sent."

func TestRedactedQuoteRefusal(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		refused bool
	}{
		{
			name:    "masked line quoted in a fenced block",
			body:    maskedFindingBody,
			refused: true,
		},
		{
			name:    "masked text quoted in an inline span",
			body:    "The test greps for `Authorization: ******`, which enshrines the bug.",
			refused: true,
		},
		{
			name:    "hive placeholder quoted as source",
			body:    "This line reads `header = [REDACTED]`, so the header is empty.",
			refused: true,
		},
		{
			name:    "unterminated fence still counts",
			body:    "Look at this:\n\n```go\nauth := \"[REDACTED]\"\n",
			refused: true,
		},
		{
			name:    "the real line passes",
			body:    "```bash\nstdin_config=\"$(printf 'header = \"Authorization: Bearer %s\"\\n' \"$TOKEN\")\"\n```\nLooks correct to me.",
			refused: false,
		},
		{
			name:    "prose about redaction is a legitimate review topic",
			body:    "pkg/logscrub redacts this before it reaches the sink; the relay does not. Worth a note in security.md.",
			refused: false,
		},
		{
			name:    "quoted banner comment is not a mask",
			body:    "The block at `foo.c:12` starts with `/**************** setup ****************/` and is dead code.",
			refused: false,
		},
		{
			name:    "quoted glob is not a mask",
			body:    "The script runs `cp ./**/*.go /tmp/` which copies more than intended.",
			refused: false,
		},
		{
			name:    "empty body",
			body:    "",
			refused: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactedQuoteRefusal(tc.body)
			if tc.refused && got == "" {
				t.Fatalf("expected a refusal for:\n%s", tc.body)
			}
			if !tc.refused && got != "" {
				t.Fatalf("unexpected refusal %q for:\n%s", got, tc.body)
			}
			if tc.refused && !strings.Contains(got, "re-read the line at the reviewed commit") {
				t.Errorf("refusal must say what to do instead, got %q", got)
			}
		})
	}
}

// A refused body is never truncated into the middle of a multi-byte rune —
// the quote is echoed back to the agent and must stay valid UTF-8.
func TestRedactedQuoteRefusalTruncatesLongQuotes(t *testing.T) {
	body := "```\n" + strings.Repeat("ü", 300) + " [REDACTED]\n```"
	got := redactedQuoteRefusal(body)
	if got == "" {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(got, "…") {
		t.Errorf("expected the echoed quote to be truncated: %q", got)
	}
}

// End to end through the relay: the review is never submitted, the request is
// quarantined so it cannot retry with the same body, and the result file tells
// the agent why.
func TestReviewRequestWatcher_RefusesReviewQuotingMaskedText(t *testing.T) {
	reviewed := 0
	srv := newReviewMockServer(t, &reviewed, nil)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	dir := withReviewDir(t)

	reqPath, err := WriteReviewRequest(dir, ReviewRequest{
		Repo: "o/r", Number: 654, Event: "comment", Body: maskedFindingBody, Agent: "reviewer",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())

	if reviewed != 0 {
		t.Errorf("a review built on masked text must not be posted; got %d", reviewed)
	}
	if _, err := os.Stat(reqPath + ".bad"); err != nil {
		t.Errorf("the request should be quarantined .bad so it cannot retry unchanged")
	}
	data, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var resp ReviewResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if resp.OK {
		t.Errorf("result should report failure: %+v", resp)
	}
	if !strings.Contains(resp.Error, "secret scrubber already masked") {
		t.Errorf("result error should name the cause, got %q", resp.Error)
	}
}

// The guard must not stand between a clean review and the PR.
func TestReviewRequestWatcher_PostsCleanReview(t *testing.T) {
	reviewed := 0
	srv := newReviewMockServer(t, &reviewed, nil)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	dir := withReviewDir(t)

	if _, err := WriteReviewRequest(dir, ReviewRequest{
		Repo: "o/r", Number: 654, Event: "comment", Agent: "reviewer",
		Body: "`launcher.sh:41` builds the header with `printf 'Authorization: Bearer %s'`; the token is sent. Looks correct to me.",
	}); err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())

	if reviewed != 1 {
		t.Errorf("clean review should be posted once, got %d", reviewed)
	}
}
