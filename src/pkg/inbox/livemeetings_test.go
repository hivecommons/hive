package inbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

type fakeLiveReader struct {
	list     []LiveMeeting
	listErr  error
	readErr  error
	handErr  error
	handed   []string
	readOnes []string
}

func (f *fakeLiveReader) EndedMeetings(context.Context) ([]LiveMeeting, error) {
	return f.list, f.listErr
}

func (f *fakeLiveReader) ReadMeeting(_ context.Context, id string) (LiveMeeting, error) {
	f.readOnes = append(f.readOnes, id)
	if f.readErr != nil {
		return LiveMeeting{}, f.readErr
	}
	for _, m := range f.list {
		if m.ID == id {
			return m, nil
		}
	}
	return LiveMeeting{}, errors.New("not found")
}

func (f *fakeLiveReader) HandOff(_ context.Context, id string) error {
	f.handed = append(f.handed, id)
	return f.handErr
}

func liveFixture() *fakeLiveReader {
	start := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	return &fakeLiveReader{list: []LiveMeeting{
		{ID: "mtg-1", Title: "Standup", Platform: "google_meet", StartedAt: start, EndedAt: start.Add(30 * time.Minute),
			Transcript: "Ana: ship friday\n", Notes: "## Decisions\n\n- ship friday", Summary: "short sync"},
		{ID: "mtg-2", Title: "Empty", Platform: "teams", StartedAt: start.Add(time.Hour)},
	}}
}

func TestLiveMeetingSourceListAndFetch(t *testing.T) {
	r := liveFixture()
	src := &LiveMeetingSource{Reader: r}
	if src.Type() != LiveMeetingType {
		t.Fatal(src.Type())
	}
	items, err := src.List(context.Background())
	if err != nil || len(items) != 2 {
		t.Fatalf("List = %+v, %v", items, err)
	}
	if items[0].ID != "mtg-2" || !items[0].Pending || items[0].DurationMS != 0 {
		t.Fatalf("newest first and empty meeting pending: %+v", items[0])
	}
	it := items[1]
	if !it.HasTranscript || it.Pending || it.DurationMS != (30*time.Minute).Milliseconds() || it.Summary != "short sync" || it.Source != "meeting" {
		t.Fatalf("item %+v", it)
	}
	c, err := src.Fetch(context.Background(), it)
	if err != nil {
		t.Fatal(err)
	}
	if c.Provenance != "hive://meetings/mtg-1" || !strings.Contains(c.Markdown, "## Transcript") || !strings.Contains(c.Markdown, "## Decisions") || c.Transcript != "Ana: ship friday" {
		t.Fatalf("content %+v", c)
	}
	if !strings.Contains(c.Text(), "ship friday") {
		t.Fatal("text")
	}
	r.readErr = errors.New("gone")
	if _, err := src.Fetch(context.Background(), it); err == nil {
		t.Fatal("want the read error")
	}
	r.listErr = errors.New("down")
	if _, err := src.List(context.Background()); err == nil {
		t.Fatal("want the list error")
	}
}

func TestLiveMeetingHandOffAfterRouteAndDismiss(t *testing.T) {
	r := liveFixture()
	src := &LiveMeetingSource{Reader: r}
	spek := &recordingDest{name: DestSpekRFC, res: Result{URL: "https://github.com/o/r/issues/9"}}
	failing := &recordingDest{name: DestDibs, err: errors.New("dibs down")}
	svc := newTestService(t, config.InboxConfig{}, src, spek, failing)
	items, _ := svc.Items(context.Background())
	var mtg1, mtg2 Item
	for _, it := range items {
		switch it.ID {
		case "mtg-1":
			mtg1 = it
		case "mtg-2":
			mtg2 = it
		}
	}
	if _, err := svc.Route(context.Background(), mtg1, Request{Destination: DestDibs}); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	if len(r.handed) != 0 {
		t.Fatal("a failed route must not hand off")
	}
	if _, err := svc.Route(context.Background(), mtg1, Request{Destination: DestSpekRFC, Repo: "o/r"}); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	if len(r.handed) != 1 || r.handed[0] != "mtg-1" {
		t.Fatalf("handed %v", r.handed)
	}
	r.handErr = errors.New("quorum down")
	if _, err := svc.Route(context.Background(), mtg2, Request{Destination: DestDismiss}); err != nil {
		t.Fatal(err)
	}
	if len(r.handed) != 2 || r.handed[1] != "mtg-2" {
		t.Fatalf("dismiss must hand off too: %v", r.handed)
	}
}

func TestHandOffIgnoresOtherSources(t *testing.T) {
	src := &fakeSource{items: []Item{{Source: "plaud", ID: "rec-1"}}, body: Content{Markdown: "x"}}
	svc := newTestService(t, config.InboxConfig{}, src)
	if _, err := svc.Route(context.Background(), Item{Source: "plaud", ID: "rec-1"}, Request{Destination: DestDismiss}); err != nil {
		t.Fatal(err)
	}
}

func TestBuiltinSourceListedOnce(t *testing.T) {
	src := &LiveMeetingSource{Reader: liveFixture()}
	svc := newTestService(t, config.InboxConfig{Sources: []string{"plaud", LiveMeetingType}}, src)
	items, err := svc.Items(context.Background())
	if err != nil || len(items) != 2 {
		t.Fatalf("items %d, %v", len(items), err)
	}
}
