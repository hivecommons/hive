package scheduler

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

const fakeQuestionSection = "QUESTION ISSUES (fake section)\n"

type fakeQuestionAutocloser struct {
	offered [][]github.Issue
	section string
}

func (f *fakeQuestionAutocloser) Offer(issues []github.Issue) {
	f.offered = append(f.offered, append([]github.Issue(nil), issues...))
}

func (f *fakeQuestionAutocloser) KickSection(issues []github.Issue) string { return f.section }

func TestQuestionAutocloserFedAndScannerToldTheContract(t *testing.T) {
	s := New(&config.Config{}, slog.Default())
	q := &fakeQuestionAutocloser{section: fakeQuestionSection}
	s.SetQuestionAutocloser(q)
	actionable := actionableWithIssues([]github.Issue{
		{Repo: "o/r", Number: 1, Title: "How do I configure the thing?", Labels: []string{"question"}},
	})
	msgs := s.BuildKickMessages(actionable, []string{"scanner", "ci-maintainer"})
	if len(q.offered) != 1 || len(q.offered[0]) != 1 || q.offered[0][0].Number != 1 {
		t.Fatalf("offered = %+v, want the classified list once per pass", q.offered)
	}
	var scanner, other string
	for _, m := range msgs {
		switch m.Agent {
		case "scanner":
			scanner = m.Message
		case "ci-maintainer":
			other = m.Message
		}
	}
	if !strings.Contains(scanner, fakeQuestionSection) {
		t.Fatalf("scanner kick lacks the answer contract:\n%s", scanner)
	}
	if other != "" && strings.Contains(other, fakeQuestionSection) {
		t.Fatal("only the scanner lane answers questions")
	}
}

func TestQuestionAutocloserAbsentOrOffLeavesKickUnchanged(t *testing.T) {
	actionable := actionableWithIssues([]github.Issue{{Repo: "o/r", Number: 1, Title: "How do I?", Labels: []string{"question"}}})

	plain := New(&config.Config{}, slog.Default())
	for _, m := range plain.BuildKickMessages(actionable, []string{"scanner"}) {
		if strings.Contains(m.Message, fakeQuestionSection) {
			t.Fatal("no auto-closer attached, yet the kick carries its section")
		}
	}

	off := New(&config.Config{}, slog.Default())
	q := &fakeQuestionAutocloser{}
	off.SetQuestionAutocloser(q)
	got := off.BuildKickMessages(actionable, []string{"scanner"})
	if len(got) != 1 || strings.Contains(got[0].Message, "QUESTION ISSUES (") {
		t.Fatalf("a disabled auto-closer (empty section) must not change the scanner kick: %+v", got)
	}
	if msg := off.addQuestionAnswerContract("scanner", "body", nil); msg != "body" {
		t.Fatalf("empty section must leave the message alone, got %q", msg)
	}
	q.section = fakeQuestionSection
	if msg := off.addQuestionAnswerContract("ci-maintainer", "body", nil); msg != "body" {
		t.Fatalf("non-scanner lane got the contract: %q", msg)
	}

	if msg := off.addQuestionAnswerContract("scanner", "", nil); msg != "" {
		t.Fatalf("empty message must stay empty, got %q", msg)
	}
	var nilCfg Scheduler
	if msg := nilCfg.addQuestionAnswerContract("scanner", "body", nil); msg != "body" {
		t.Fatalf("nil config must leave the message alone, got %q", msg)
	}
	nilCfg.offerQuestions(nil)
}
