package rfcnotify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

const rfc15869Body = `Held RFC. This is a design proposal for discussion. It carries ` + "`hold`" + ` so no hive agent picks it up until maintainers settle **Decisions for maintainers**.

| Field | Value |
| --- | --- |
| Status | Proposal, not accepted |
| Target | v5 |
| Scope | ` + "`src/pkg/rfcnotify`" + ` (detector, sweeper, ` + "`Format`" + `), ` + "`src/pkg/discord/bot.go`" + ` |
| Out of scope | Two-way sync, slash commands |
| Builds on | #14616 / #14617 (RFC lifecycle notices, merged on v5 2026-10-27) |
| Channel | ` + "`#architecture-rfcs`" + `, https://discord.com/channels/1539670512744136715/1540404487099777136 |

## Summary

RFCs already reach Discord automatically. #14617 shipped ` + "`pkg/rfcnotify`" + `: set ` + "`notifications.discord.rfc_channel_id`" + ` and the hive polls every 3 min. It posts one plain line when an RFC issue is opened, when it closes, and when its hold comes off.

This RFC makes that feed something people read and discuss rather than scroll past:

- a summary card instead of a bare link;
- one Discord thread per RFC;

## Decisions for maintainers

1. **Defaults.** Make ` + "`card`" + ` and threads the default on upgrade (proposed), or keep today's one-liner and opt in?
2. **Text channel or Forum channel.** Keep ` + "`#architecture-rfcs`" + ` as a text channel with threads (proposed), or move to a Forum channel?
3. **Single announcer.** The channel-history idempotency check (proposed), or designate one hive or the hub as the announcer?
4. **Backlink comment.** Should the bot comment on GitHub issues it did not open?
5. **Comment digest.** Daily per-thread digest: in scope, or later?
`

func TestParseRFC_RFC15869(t *testing.T) {
	meta := ParseRFC(rfc15869Body)
	if meta.Empty() {
		t.Fatal("RFC #15869 body should parse, but Empty() was true")
	}
	if meta.Target != "v5" {
		t.Errorf("Target = %q, want %q", meta.Target, "v5")
	}
	if meta.Status != "Proposal, not accepted" {
		t.Errorf("Status = %q, want %q", meta.Status, "Proposal, not accepted")
	}
	if meta.OpenDecisions != 5 {
		t.Errorf("OpenDecisions = %d, want 5", meta.OpenDecisions)
	}
	if len(meta.RelatedRFCLinks) != 2 || meta.RelatedRFCLinks[0] != "#14616" || meta.RelatedRFCLinks[1] != "#14617" {
		t.Errorf("RelatedRFCLinks = %v, want [#14616 #14617]", meta.RelatedRFCLinks)
	}
	if !strings.HasPrefix(meta.Summary, "RFCs already reach Discord automatically.") {
		t.Errorf("Summary = %q, want prefix RFCs already reach...", meta.Summary)
	}
	if len([]rune(meta.Summary)) > 300 {
		t.Errorf("Summary runes = %d, want <= 300", len([]rune(meta.Summary)))
	}
}

func TestParseRFC_DecisionsStrikethroughAndCheckboxes(t *testing.T) {
	body := `
| Field | Value |
| --- | --- |
| Target | v6 |

## Summary
A short summary of changes.

## Decisions for maintainers

1. Open numbered question
2. ~~Resolved numbered question~~
- [ ] Open checkbox decision
- [x] Completed checkbox decision
- [X] Also completed checkbox decision
- [ ] ~~Struck through checkbox decision~~
3. Another open question
4. [x] Completed numbered decision
5) [X] Also completed numbered decision
(6) [x] Completed parenthesized decision
7. [ ] Open numbered checkbox
`
	meta := ParseRFC(body)
	if meta.Target != "v6" {
		t.Errorf("Target = %q, want v6", meta.Target)
	}
	// Items 1, checkbox open, item 3, item 7 = 4 open decisions
	if meta.OpenDecisions != 4 {
		t.Errorf("OpenDecisions = %d, want 4", meta.OpenDecisions)
	}
}

func TestParseRFC_SummaryScrubbingAndTruncation(t *testing.T) {
	fakeToken := "ghp_" + strings.Repeat("a", 36)
	longText := strings.Repeat("Long text sentence. ", 30) // ~600 runes
	body := `
| Field | Value |
| --- | --- |
| Status | In review |

## Summary
` + fakeToken + " " + longText

	meta := ParseRFC(body)
	if strings.Contains(meta.Summary, fakeToken) {
		t.Error("Summary must not contain raw credential token")
	}
	if !strings.Contains(meta.Summary, "[REDACTED]") {
		t.Errorf("Summary must contain [REDACTED], got: %q", meta.Summary)
	}
	if len([]rune(meta.Summary)) > 300 {
		t.Errorf("Summary length = %d runes, want <= 300", len([]rune(meta.Summary)))
	}
}

func TestParseRFC_MalformedBody(t *testing.T) {
	bodies := []string{
		"",
		"Just a normal bug description with no RFC table and no summary heading.",
		"# Not an RFC\nSome text here\nAnother line",
	}
	for _, b := range bodies {
		meta := ParseRFC(b)
		if !meta.Empty() {
			t.Errorf("body %q should be Empty(), got %+v", b, meta)
		}
	}
}

func TestCard_FieldsAndMentionsSafety(t *testing.T) {
	ev := Event{
		Kind: KindOpened,
		Issue: Issue{
			Repo:   "hivecommons/hive",
			Number: 15869,
			Title:  "📝 RFC: Richer RFC publishing @everyone <@&12345>",
			URL:    "https://github.com/hivecommons/hive/issues/15869",
			Author: "hanthor",
			State:  "open",
			Labels: []string{"rfc", "hold"},
			Body:   rfc15869Body,
		},
	}
	meta := ParseRFC(rfc15869Body)
	card := Card(ev, meta, false)

	if card.Title != "#15869 📝 RFC: Richer RFC publishing @everyone <@&12345>" {
		t.Errorf("Title = %q", card.Title)
	}
	if card.URL != "https://github.com/hivecommons/hive/issues/15869" {
		t.Errorf("URL = %q", card.URL)
	}
	if card.Author == nil || card.Author.Name != "hanthor" {
		t.Errorf("Author = %+v, want hanthor", card.Author)
	}
	if card.Author.IconURL != "https://github.com/hanthor.png" {
		t.Errorf("Author.IconURL = %q, want https://github.com/hanthor.png", card.Author.IconURL)
	}
	if card.Color != ColorHeld {
		t.Errorf("Color = %x, want ColorHeld (%x)", card.Color, ColorHeld)
	}

	fieldMap := make(map[string]string)
	for _, f := range card.Fields {
		fieldMap[f.Name] = f.Value
	}
	if fieldMap["Target"] != "v5" {
		t.Errorf("Field Target = %q, want v5", fieldMap["Target"])
	}
	if fieldMap["Status"] != "Proposal, not accepted" {
		t.Errorf("Field Status = %q, want Proposal, not accepted", fieldMap["Status"])
	}
	if fieldMap["Decisions"] != "5 open" {
		t.Errorf("Field Decisions = %q, want '5 open'", fieldMap["Decisions"])
	}
	if fieldMap["Related"] != "#14616, #14617" {
		t.Errorf("Field Related = %q, want '#14616, #14617'", fieldMap["Related"])
	}

	// Test with includeRepo = true
	cardRepo := Card(ev, meta, true)
	if !strings.HasPrefix(cardRepo.Title, "hivecommons/hive#15869 ") {
		t.Errorf("cardRepo.Title = %q, want repo prefix", cardRepo.Title)
	}
}

func TestCard_FieldScrubbing(t *testing.T) {
	fakeToken := "ghp_" + strings.Repeat("b", 36)
	ev := Event{
		Issue: Issue{
			Number: 42,
			Title:  "RFC: Test Title",
			URL:    "https://example.com/42",
		},
	}
	meta := RFCMeta{
		Target:          "target with " + fakeToken,
		Status:          "status with " + fakeToken,
		RelatedRFCLinks: []string{"related with " + fakeToken},
	}
	card := Card(ev, meta, false)
	for _, f := range card.Fields {
		if strings.Contains(f.Value, fakeToken) {
			t.Errorf("field %s contains unscrubbed token: %q", f.Name, f.Value)
		}
	}
}

func TestCard_ColorsByState(t *testing.T) {
	cases := []struct {
		name      string
		kind      Kind
		open      bool
		reason    string
		labels    []string
		wantColor int
	}{
		{
			name:      "held open",
			kind:      KindOpened,
			open:      true,
			labels:    []string{"rfc", "hold"},
			wantColor: ColorHeld, // grey
		},
		{
			name:      "needs decision",
			kind:      KindOpened,
			open:      true,
			labels:    []string{"rfc", "stage/045-needs-decision"},
			wantColor: ColorNeedsDecision, // amber
		},
		{
			name:      "legacy needs-decision",
			kind:      KindOpened,
			open:      true,
			labels:    []string{"rfc", "needs-decision"},
			wantColor: ColorNeedsDecision, // amber
		},
		{
			name:      "ready to implement",
			kind:      KindReady,
			open:      true,
			labels:    []string{"rfc"},
			wantColor: ColorReady, // green
		},
		{
			name:      "actionable without hold",
			kind:      KindOpened,
			open:      true,
			labels:    []string{"rfc", "stage/010-actionable"},
			wantColor: ColorReady, // green
		},
		{
			name:      "closed completed",
			kind:      KindClosed,
			open:      false,
			reason:    "completed",
			labels:    []string{"rfc"},
			wantColor: ColorCompleted, // blue
		},
		{
			name:      "closed not planned",
			kind:      KindClosed,
			open:      false,
			reason:    "not_planned",
			labels:    []string{"rfc"},
			wantColor: ColorNotPlanned, // dark
		},
		{
			name:      "doc accepted PR",
			kind:      KindDocAccepted,
			open:      false,
			labels:    []string{"rfc"},
			wantColor: ColorCompleted, // blue
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := "open"
			if !tc.open {
				state = "closed"
			}
			ev := Event{
				Kind: tc.kind,
				Issue: Issue{
					Number:      1,
					Title:       "RFC test",
					State:       state,
					StateReason: tc.reason,
					Labels:      tc.labels,
				},
			}
			color := CardColor(ev)
			if color != tc.wantColor {
				t.Errorf("CardColor = %x, want %x", color, tc.wantColor)
			}
		})
	}
}

type recordPublisher struct {
	posted       []string
	cards        []Embed
	cardContents []string
	fail         bool
}

func (p *recordPublisher) Post(content string) error {
	if p.fail {
		return errors.New("post error")
	}
	p.posted = append(p.posted, content)
	return nil
}

func (p *recordPublisher) PostCard(content string, embed Embed) (string, error) {
	if p.fail {
		return "", errors.New("card post error")
	}
	p.cards = append(p.cards, embed)
	p.cardContents = append(p.cardContents, content)
	return "msg-card-1", nil
}

func TestSweeperCardFormat_PostsCardAndFallsBackOnMalformed(t *testing.T) {
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	sw := NewSweeper(Options{
		Format:    FormatCard,
		Announcer: true,
		Now:       func() time.Time { return now },
		Logger:    quiet,
	})

	// Seed sweeper
	src := &fakeSource{
		open: []Issue{
			{Number: 1, Title: "Existing RFC", State: "open", Labels: []string{"rfc"}},
		},
	}
	pub := &recordPublisher{}
	if res, err := sw.Run(context.Background(), src, pub); err != nil || !res.Seeded {
		t.Fatalf("seed failed: %+v, err: %v", res, err)
	}

	// Poll 1: Well-formed RFC #2, and malformed RFC #3
	now = now.Add(5 * time.Minute)
	src.updated = []Issue{
		{
			Number:    2,
			Title:     "RFC: Well-formed",
			URL:       "https://x/2",
			State:     "open",
			Labels:    []string{"rfc", "hold"},
			Body:      rfc15869Body,
			CreatedAt: now,
		},
		{
			Number:    3,
			Title:     "RFC: Malformed body",
			URL:       "https://x/3",
			State:     "open",
			Labels:    []string{"rfc"},
			Body:      "No table, no summary heading here.",
			CreatedAt: now,
		},
	}

	res, err := sw.Run(context.Background(), src, pub)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if len(res.Posted) != 2 {
		t.Fatalf("len(res.Posted) = %d, want 2", len(res.Posted))
	}
	// Card #2 should be posted as card embed
	if len(pub.cards) != 1 {
		t.Fatalf("len(pub.cards) = %d, want 1", len(pub.cards))
	}
	if !strings.HasPrefix(pub.cardContents[0], "RFC opened: #2 RFC: Well-formed - <https://x/2>") {
		t.Errorf("card content = %q", pub.cardContents[0])
	}
	if pub.cards[0].Title != "#2 RFC: Well-formed" {
		t.Errorf("card title = %q, want '#2 RFC: Well-formed'", pub.cards[0].Title)
	}
	if pub.cards[0].Color != ColorHeld {
		t.Errorf("card color = %x, want ColorHeld", pub.cards[0].Color)
	}

	// Malformed #3 should fall back to plain line post
	if len(pub.posted) != 1 {
		t.Fatalf("len(pub.posted) = %d, want 1", len(pub.posted))
	}
	if !strings.HasPrefix(pub.posted[0], "RFC opened: #3 RFC: Malformed body - <https://x/3>") {
		t.Errorf("pub.posted[0] = %q", pub.posted[0])
	}
}

func TestSweeperCardFormat_PostCardFailureRollsBack(t *testing.T) {
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	sw := NewSweeper(Options{
		Format:    FormatCard,
		Announcer: true,
		Now:       func() time.Time { return now },
		Logger:    quiet,
	})

	src := &fakeSource{
		open: []Issue{
			{Number: 1, Title: "Existing RFC", State: "open", Labels: []string{"rfc"}},
		},
	}
	pub := &recordPublisher{}
	if _, err := sw.Run(context.Background(), src, pub); err != nil {
		t.Fatalf("seed failed: %v", err)
	}

	// Fail the card post
	pub.fail = true
	now = now.Add(5 * time.Minute)
	src.updated = []Issue{
		{
			Number:    2,
			Title:     "RFC: Card fail",
			URL:       "https://x/2",
			State:     "open",
			Labels:    []string{"rfc"},
			Body:      rfc15869Body,
			CreatedAt: now,
		},
	}

	res, err := sw.Run(context.Background(), src, pub)
	if err != nil {
		t.Fatalf("run returned error: %v", err)
	}
	if res.Errors != 1 {
		t.Errorf("res.Errors = %d, want 1", res.Errors)
	}

	// Retry on next poll without failure: should succeed
	pub.fail = false
	now = now.Add(5 * time.Minute)
	res2, err := sw.Run(context.Background(), src, pub)
	if err != nil {
		t.Fatalf("run retry returned error: %v", err)
	}
	if len(res2.Posted) != 1 || len(pub.cards) != 1 {
		t.Fatalf("retry should post card: posted=%d cards=%d", len(res2.Posted), len(pub.cards))
	}
}

func TestSweeperCardFormat_PreservesTransitions(t *testing.T) {
	fresh := rfc(2, "open")
	fresh.CreatedAt = t0.Add(time.Second)
	proposal := rfcPR(2, "📝 proposal: cards", t0.Add(time.Second))
	for _, tc := range []struct {
		name   string
		before []Issue
		after  Issue
		prefix string
	}{
		{"opened", nil, fresh, "RFC opened: "},
		{"ready", []Issue{rfc(2, "open", "hold")}, rfc(2, "open"), "RFC ready to implement (hold removed): "},
		{"completed", []Issue{rfc(2, "open")}, withReason(rfc(2, "open"), "completed"), "RFC closed (completed): "},
		{"not planned", []Issue{rfc(2, "open")}, withReason(rfc(2, "open"), "not_planned"), "RFC closed (not planned): "},
		{"doc proposed", nil, proposal, "RFC doc proposed: "},
		{"doc accepted", []Issue{rfc(1, "open")}, merged(proposal, t0.Add(time.Second)), "RFC doc accepted (RFC #1): "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, fallback := range []bool{false, true} {
				now := t0
				sw := NewSweeper(Options{Format: FormatCard, Announcer: true, IncludeRepo: true, Logger: quiet, Now: func() time.Time { return now }})
				src := &fakeSource{open: tc.before}
				pub := &recordPublisher{}
				var publisher Publisher = pub
				if fallback {
					publisher = Poster(pub.Post)
				}
				if _, err := sw.Run(context.Background(), src, publisher); err != nil {
					t.Fatal(err)
				}
				after := tc.after
				after.Body = "Refs #1\n\n## Summary\nRFC summary."
				src.updated = []Issue{after}
				now = now.Add(DefaultInterval)
				res, err := sw.Run(context.Background(), src, publisher)
				if err != nil || len(res.Posted) != 1 {
					t.Fatalf("fallback=%v: result=%+v err=%v", fallback, res, err)
				}
				contents := pub.cardContents
				if fallback {
					contents = pub.posted
				} else if len(pub.cards) != 1 {
					t.Fatalf("cards = %d, want 1", len(pub.cards))
				}
				if len(contents) != 1 || !strings.HasPrefix(contents[0], tc.prefix+"hivecommons/hive#2 ") || !strings.Contains(contents[0], "<"+after.URL+">") {
					t.Fatalf("fallback=%v: content=%v, want transition %q, repo and URL", fallback, contents, tc.prefix)
				}
			}
		})
	}
}

func TestParseRFCDecisionsSubHeadingsAndLinks(t *testing.T) {
	body := "## Summary\n\nSee [click](http://evil.example) now.\n\n## Decisions for maintainers\n\n### Context\n\n### 1. First\n\n1. Pick A\n2. Pick B\n\n## Other\n\n3. not counted\n"
	m := ParseRFC(body)
	if m.OpenDecisions != 2 {
		t.Fatalf("OpenDecisions = %d, want 2", m.OpenDecisions)
	}
	if strings.Contains(m.Summary, "](") {
		t.Fatalf("summary keeps masked link: %q", m.Summary)
	}
}

func TestCard_BotAuthorOmitsIconURL(t *testing.T) {
	ev := Event{Kind: KindOpened, Issue: Issue{
		Repo: "hivecommons/hive", Number: 1, Title: "t", Author: "hivecommons-hive[bot]",
		Body: rfc15869Body,
	}}
	card := Card(ev, ParseRFC(rfc15869Body), false)
	if card.Author == nil || card.Author.Name != "hivecommons-hive[bot]" {
		t.Fatalf("Author = %+v", card.Author)
	}
	if card.Author.IconURL != "" {
		t.Errorf("IconURL = %q, want empty for [bot] login", card.Author.IconURL)
	}
}
