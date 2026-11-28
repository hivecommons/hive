package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

const forgeVerdictHead = "0123456789abcdef0123456789abcdef01234567"

type verdictHarness struct {
	*relayHarness
	api       *fakeReadAPI
	reportDir string
	wakes     []string
}

// newVerdictHarness is a forge-only relay whose review queue serves
// record_verdict (hivecommons/hive#20292): "reviewer" is the hive's reviewer
// lane, "worker" is an authorized lane that is not, and the PR is read from
// a fake Forgejo.
func newVerdictHarness(t *testing.T) *verdictHarness {
	t.Helper()
	h := &verdictHarness{relayHarness: newRelayHarness(t), api: &fakeReadAPI{
		pull: forge.PullView{Number: 7, Title: "fix: x", Body: "Fixes #1", State: "open", Author: "someone",
			Labels: []string{"lgtm"}, HeadRef: "fix-1", HeadSHA: forgeVerdictHead, Base: "main"},
	}}
	h.reportDir = redirectReviewPaths(t)
	prevLegacy := review.LegacyReviewDispatchStatePath
	review.LegacyReviewDispatchStatePath = filepath.Join(t.TempDir(), review.ReviewDispatchStateFile)
	t.Cleanup(func() { review.LegacyReviewDispatchStatePath = prevLegacy })
	prevAPI := forgeReadAPIForRepo
	t.Cleanup(func() { forgeReadAPIForRepo = prevAPI })
	forgeReadAPIForRepo = func(*config.Config, string) (forge.ReadAPI, error) { return h.api, nil }

	h.cfg.Review.ReviewerAgents = []string{"reviewer"}
	uid := os.Getuid()
	h.relay.withReviewRelay(func(agent string, fileUID int) error {
		if (agent != "reviewer" && agent != "worker") || fileUID != uid {
			return errors.New("not this agent's request")
		}
		return nil
	}, func(repo string, number int) { h.wakes = append(h.wakes, repo) })
	return h
}

func forgeVerdictReport(t *testing.T, repo string, number int, head string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"lane": "review-swarm", "kind": "review", "summary": "judged",
		"findings": []any{}, "prs_opened": []any{}, "beads_filed": []any{},
		"perspective": "correctness", "verdict": "approve",
		"repo": repo, "number": number, "head_sha": head,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func (h *verdictHarness) verdictRequest(t *testing.T, agent string) github.ReviewRequest {
	return github.ReviewRequest{Repo: "widgets", Number: 7, Event: github.ReviewEventRecordVerdict, Agent: agent,
		Report: forgeVerdictReport(t, "widgets", 7, forgeVerdictHead)}
}

func (h *verdictHarness) reports(t *testing.T) []review.PerspectiveReport {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(h.reportDir, review.ReviewReportFilePrefix+"*"))
	var out []review.PerspectiveReport
	for _, m := range matches {
		raw, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		var r review.PerspectiveReport
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		out = append(out, r)
	}
	return out
}

// A reviewer's record_verdict on a forge-only hive writes the verdict report
// review.Collect reads, stamped with the body digest of the PR the forge
// returns, and answers OK.
func TestForgeRelayRecordsVerdict(t *testing.T) {
	h := newVerdictHarness(t)
	path := writeRelayRequest(t, h.relay.reviewDir, "v.json", h.verdictRequest(t, "reviewer"))
	h.relay.scanOnce(context.Background())

	var rr github.ReviewResponse
	readRelayResult(t, path, &rr)
	if !rr.OK || rr.Number != 7 || rr.State != github.ReviewEventRecordVerdict || rr.Error != "" {
		t.Fatalf("resp = %+v, want OK record_verdict", rr)
	}
	assertExists(t, path, false)
	got := h.reports(t)
	if len(got) != 1 {
		t.Fatalf("recorded %d reports, want 1", len(got))
	}
	if got[0].HeadSHA != forgeVerdictHead || got[0].Verdict != review.VerdictApprove {
		t.Errorf("report = %+v", got[0])
	}
	if want := review.PullRequestBodyDigest("fix: x", "Fixes #1"); got[0].BodyDigest != want {
		t.Errorf("BodyDigest = %q, want %q (read from the forge)", got[0].BodyDigest, want)
	}
	if len(h.writer.comments)+len(h.writer.specs)+len(h.writer.labels) != 0 {
		t.Errorf("record_verdict wrote to the forge: %+v", h.writer)
	}
}

// The advisory rule holds on the forge relay: a lane that is not a reviewer,
// and the PR's own author, record nothing.
func TestForgeRelayVerdictAdvisoryRuleRefuses(t *testing.T) {
	t.Run("non-reviewer", func(t *testing.T) {
		h := newVerdictHarness(t)
		path := writeRelayRequest(t, h.relay.reviewDir, "v.json", h.verdictRequest(t, "worker"))
		h.relay.scanOnce(context.Background())
		assertExists(t, path, false)
		if got := h.reports(t); len(got) != 0 {
			t.Fatalf("non-reviewer verdict was recorded: %+v", got)
		}
	})
	t.Run("author", func(t *testing.T) {
		h := newVerdictHarness(t)
		row := review.PendingReview{Repo: "widgets", Number: 7, HeadSHA: forgeVerdictHead, Perspective: review.PerspectiveCorrectness,
			Agent: "reviewer", AuthorAgent: "reviewer", Dispatched: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
		if err := review.WriteDispatchState("", review.DispatchState{Pending: []review.PendingReview{row}}); err != nil {
			t.Fatal(err)
		}
		path := writeRelayRequest(t, h.relay.reviewDir, "v.json", h.verdictRequest(t, "reviewer"))
		h.relay.scanOnce(context.Background())
		assertExists(t, path, false)
		if got := h.reports(t); len(got) != 0 {
			t.Fatalf("the PR author's own verdict was recorded: %+v", got)
		}
		if len(h.wakes) != 0 {
			t.Errorf("a refused verdict woke the governor: %v", h.wakes)
		}
	})
}

// The admission checks before the writer: the file owner must be the claimed
// agent, review.require_labels must admit the PR, and a relay with no review
// authorizer denies (fail closed).
func TestForgeRelayVerdictAdmissionDenies(t *testing.T) {
	t.Run("wrong agent", func(t *testing.T) {
		h := newVerdictHarness(t)
		path := writeRelayRequest(t, h.relay.reviewDir, "v.json", h.verdictRequest(t, "intruder"))
		h.relay.scanOnce(context.Background())
		var rr github.ReviewResponse
		readRelayResult(t, path, &rr)
		if rr.OK {
			t.Fatalf("resp = %+v, want denied", rr)
		}
		assertExists(t, path+".denied", true)
	})
	t.Run("require_labels", func(t *testing.T) {
		h := newVerdictHarness(t)
		h.cfg.Review.RequireLabels = []string{"approved-for-review"}
		path := writeRelayRequest(t, h.relay.reviewDir, "v.json", h.verdictRequest(t, "reviewer"))
		h.relay.scanOnce(context.Background())
		var rr github.ReviewResponse
		readRelayResult(t, path, &rr)
		if rr.OK {
			t.Fatalf("resp = %+v, want denied by review.require_labels", rr)
		}
		assertExists(t, path+".denied", true)
		if got := h.reports(t); len(got) != 0 {
			t.Fatalf("verdict recorded past review.require_labels: %+v", got)
		}
	})
	t.Run("no authorizer", func(t *testing.T) {
		h := newVerdictHarness(t)
		h.relay.reviewAuthz = nil
		path := writeRelayRequest(t, h.relay.reviewDir, "v.json", h.verdictRequest(t, "reviewer"))
		h.relay.scanOnce(context.Background())
		assertExists(t, path+".denied", true)
		if got := h.reports(t); len(got) != 0 {
			t.Fatalf("verdict recorded with no authorizer: %+v", got)
		}
	})
}
