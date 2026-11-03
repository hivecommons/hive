package rfcnotify

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func rfcPR(n int, title string, created time.Time, files ...string) Issue {
	return Issue{
		Repo: "hivecommons/hive", Number: n, Title: title, Author: "hanthor",
		URL: fmt.Sprintf("https://github.com/hivecommons/hive/pull/%d", n), State: "open",
		CreatedAt: created, Files: files, IsPR: true,
	}
}

func merged(pr Issue, at time.Time) Issue {
	pr.State, pr.MergedAt = "closed", at
	return pr
}

func TestIsRFCPR(t *testing.T) {
	for _, title := range []string{"📝 proposal: Discord RFC cards", "📝 RFC: threads", "📝 rfc - lowercase"} {
		if !IsRFCPR(title, nil) {
			t.Errorf("IsRFCPR(%q) = false, want true", title)
		}
	}
	for _, title := range []string{"", "✨ feat: proposal: no", "RFC: an issue-style title", "docs: 📝 proposal: later"} {
		if IsRFCPR(title, nil) {
			t.Errorf("IsRFCPR(%q) = true, want false", title)
		}
	}
	for _, files := range [][]string{{"docs/rfc-discord.md"}, {"README.md", "src/docs/rfc-cards.md"}} {
		if !IsRFCPR("✨ feat: x", files) {
			t.Errorf("IsRFCPR(files %v) = false, want true", files)
		}
	}
	for _, files := range [][]string{{"docs/rfc.md"}, {"docs/rfcs/x.md"}, {"x/docs/rfc-a.md"}, {"docs/rfc-a.txt"}, {"src/docs/notifications.md"}} {
		if IsRFCPR("✨ feat: x", files) {
			t.Errorf("IsRFCPR(files %v) = true, want false", files)
		}
	}
}

func TestFirstHeadingAndLinkedIssueKeys(t *testing.T) {
	if got := FirstHeading("---\nstatus: draft\n---\n\n## Discord RFC cards ##\n# later"); got != "Discord RFC cards" {
		t.Errorf("FirstHeading = %q", got)
	}
	if got := FirstHeading("no heading\n#hashtag"); got != "" {
		t.Errorf("FirstHeading without heading = %q", got)
	}
	body := "Closes #15942\nPart of #15869\nRefs: other/repo#3 and https://github.com/hivecommons/hive/issues/12062, closes #15942 again. Not #7."
	want := []string{"hivecommons/hive#15942", "hivecommons/hive#15869", "other/repo#3", "hivecommons/hive#12062"}
	if got := LinkedIssueKeys("hivecommons/hive", body); !reflect.DeepEqual(got, want) {
		t.Errorf("LinkedIssueKeys = %v, want %v", got, want)
	}
}

func TestFormatPREvents(t *testing.T) {
	pr := rfcPR(20, "📝 proposal: cards", t0)
	pr.URL = "https://x/20"
	if got := Format(Event{Kind: KindOpened, Issue: pr}, false); got != "RFC doc proposed: #20 📝 proposal: cards - <https://x/20> (by @hanthor)" {
		t.Errorf("opened = %q", got)
	}
	pr.DocTitle = "Discord RFC cards"
	if got := Format(Event{Kind: KindOpened, Issue: pr}, false); !strings.HasPrefix(got, "RFC doc proposed: #20 Discord RFC cards - ") {
		t.Errorf("opened with doc title = %q", got)
	}
	if got := Format(Event{Kind: KindDocAccepted, Issue: pr, LinkedRFC: 15869}, false); got != "RFC doc accepted (RFC #15869): #20 Discord RFC cards - <https://x/20>" {
		t.Errorf("accepted = %q", got)
	}
	if got := Format(Event{Kind: KindDocAccepted, Issue: pr}, false); got != "RFC doc accepted: #20 Discord RFC cards - <https://x/20>" {
		t.Errorf("accepted, no link = %q", got)
	}
}

func TestDetectorRFCPRs(t *testing.T) {
	d := NewDetector()
	existing := rfcPR(10, "📝 proposal: already open", t0.Add(-time.Hour))
	d.Seed([]Issue{rfc(1, "open"), existing}, t0)
	if !d.Known(existing.Key()) {
		t.Fatal("seed must record an existing open proposal PR")
	}

	t1 := t0.Add(3 * time.Minute)
	doc := rfcPR(11, "✨ feat: add the cards doc", t1.Add(-time.Minute), "docs/rfc-cards.md")
	proposal := rfcPR(12, "📝 proposal: threads", t1.Add(-time.Minute))
	unrelated := rfcPR(13, "🐛 fix: typo", t1.Add(-time.Minute), "README.md")
	// An old proposal PR the seed did not list (created before the
	// watermark) is recorded, not announced.
	old := rfcPR(14, "📝 RFC: old", t0.Add(-24*time.Hour))
	evs := d.Observe([]Issue{existing, doc, proposal, unrelated, old}, t1)
	if !reflect.DeepEqual(kinds(evs), []Kind{KindOpened, KindOpened}) || evs[0].Issue.Number != 11 || evs[1].Issue.Number != 12 {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].Thread != doc.Key() {
		t.Errorf("opened thread = %q, want the PR's own", evs[0].Thread)
	}
	if d.Known(unrelated.Key()) || !d.Known(old.Key()) {
		t.Error("unrelated PR must be ignored and the old proposal recorded")
	}
	// Seen again: announced once.
	if evs := d.Observe([]Issue{doc, proposal, unrelated}, t1.Add(time.Minute)); len(evs) != 0 {
		t.Fatalf("re-observe = %+v", evs)
	}

	// Merging the proposal posts into the linked RFC's thread.
	t2 := t1.Add(3 * time.Minute)
	proposal.Body = "Closes #99\nPart of #1"
	if evs := d.Observe([]Issue{merged(proposal, t2.Add(-time.Second))}, t2); len(evs) != 1 || evs[0].Kind != KindDocAccepted || evs[0].Thread != "hivecommons/hive#1" || evs[0].LinkedRFC != 1 {
		t.Fatalf("merge = %+v", evs)
	}
	// No linked RFC known: the PR's own thread. Closed unmerged: silent.
	if evs := d.Observe([]Issue{merged(doc, t2), merged(existing, t2)}, t2.Add(time.Minute)); len(evs) != 2 || evs[0].Issue.Number != 10 || evs[1].Thread != doc.Key() || evs[1].LinkedRFC != 0 {
		t.Fatalf("merge without link = %+v", evs)
	}
	closedOld := old
	closedOld.State = "closed"
	if evs := d.Observe([]Issue{closedOld}, t2.Add(2*time.Minute)); len(evs) != 0 {
		t.Fatalf("closed unmerged = %+v", evs)
	}
	// An unknown PR merged before the watermark is not backfilled; one
	// merged after it is announced.
	t3 := t2.Add(5 * time.Minute)
	stale := merged(rfcPR(15, "📝 RFC: stale", t0.Add(-48*time.Hour)), t0.Add(-time.Hour))
	fresh := merged(rfcPR(16, "📝 RFC: fresh", t0.Add(-48*time.Hour)), t3.Add(-time.Second))
	if evs := d.Observe([]Issue{stale, fresh}, t3); len(evs) != 1 || evs[0].Issue.Number != 16 || evs[0].Kind != KindDocAccepted {
		t.Fatalf("unknown merged = %+v", evs)
	}
}

type prFakeSource struct {
	fakeSource
	prs     []Issue
	prPolls int
}

func (f *prFakeSource) ListPRsUpdatedSince(context.Context, time.Time) ([]Issue, error) {
	f.prPolls++
	return f.prs, nil
}

func TestSweeperRFCPRsRichModeOnly(t *testing.T) {
	now := t0
	clock := func() time.Time { return now }
	var posted []string
	post := func(s string) error { posted = append(posted, s); return nil }
	ctx := context.Background()

	// Line mode never asks for pull requests.
	src := &prFakeSource{}
	line := NewSweeper(Options{Logger: quiet, Now: clock, Interval: time.Minute})
	_, _ = line.Run(ctx, src, Poster(post))
	now = now.Add(2 * time.Minute)
	src.prs = []Issue{rfcPR(5, "📝 proposal: x", now.Add(-time.Second))}
	if res, err := line.Run(ctx, src, Poster(post)); err != nil || src.prPolls != 0 || len(res.Posted) != 0 {
		t.Fatalf("line mode: %+v err=%v prPolls=%d", res, err, src.prPolls)
	}

	// Rich mode, designated announcer: a proposal PR is announced once.
	now = t0
	src = &prFakeSource{}
	rich := NewSweeper(Options{Logger: quiet, Now: clock, Interval: time.Minute, Format: FormatCard, Announcer: true})
	_, _ = rich.Run(ctx, src, Poster(post))
	now = now.Add(2 * time.Minute)
	priv := rfcPR(6, "📝 proposal: secret", now.Add(-time.Second))
	priv.Repo, priv.Private = "o/private", true
	src.prs = []Issue{rfcPR(5, "📝 proposal: x", now.Add(-time.Second)), priv}
	res, err := rich.Run(ctx, src, Poster(post))
	if err != nil || src.prPolls != 1 || len(posted) != 1 || !strings.HasPrefix(posted[0], "RFC doc proposed: #5 📝 proposal: x") || res.Withheld != 1 {
		t.Fatalf("rich mode: %+v err=%v posted=%v", res, err, posted)
	}
	now = now.Add(2 * time.Minute)
	if res, _ := rich.Run(ctx, src, Poster(post)); len(res.Posted) != 0 || len(posted) != 1 {
		t.Fatalf("second poll re-announced: %+v", res)
	}
}
