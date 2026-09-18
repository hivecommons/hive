package mention

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestResponderRepliesOnArchivedMentionRun(t *testing.T) {
	store := mustStore(t)
	ev := Event{Repo: "org/repo", Kind: "issue", Number: 7, NodeID: "N", CommentID: 9, HTMLURL: "u", Author: "alice"}
	source := mentionKickSource(ev)
	if err := store.RecordPending("scanner", ev, source, time.Now()); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGH{app: "hive[bot]"}
	r := NewResponder(store, func() GitHub { return gh }, func() []AgentInfo {
		return []AgentInfo{{Name: "scanner", Enabled: true, Converse: true}}
	}, config.ReviewBotsConfig{MaxAttemptsPerThread: 2}, nil)

	r.HandleAgentEvent("scanner", "kick-delivered", "ignored")
	if gh.comment != "" {
		t.Fatalf("non-archive event replied: %q", gh.comment)
	}
	r.HandleAgentEvent("scanner", "kick-delivered", source)
	r.HandleAgentEvent("other", "kick-log-archived", "archive source=mention")
	if gh.comment != "" {
		t.Fatalf("other agent replied: %q", gh.comment)
	}
	r.HandleAgentEvent("scanner", "kick-log-archived", "archive")
	if gh.comment != "" {
		t.Fatalf("non-mention archive replied: %q", gh.comment)
	}
	r.HandleAgentEvent("scanner", "kick-log-archived", "archive source="+source)
	if !strings.Contains(gh.comment, "scanner") || !strings.Contains(gh.comment, "finished") {
		t.Fatalf("completion comment = %q", gh.comment)
	}
	if _, ok := store.ActiveForAgent("scanner"); ok {
		t.Fatal("active mention was not cleared after reply")
	}
}

func TestResponderPromotesOnlyDeliveredMentionKicks(t *testing.T) {
	store := mustStore(t)
	ev := Event{Repo: "org/repo", Number: 1, NodeID: "N"}
	source := mentionKickSource(ev)
	if err := store.RecordPending("scanner", ev, source, time.Now()); err != nil {
		t.Fatal(err)
	}
	r := NewResponder(store, nil, nil, nil, nil)
	r.HandleAgentEvent("scanner", "kick-delivered", "governor")
	if _, ok := store.ActiveForAgent("scanner"); ok {
		t.Fatal("non-mention delivery promoted pending mention")
	}
	r.HandleAgentEvent("scanner", "kick-log-archived", "archive source=mention")
	if _, ok := store.ActiveForAgent("scanner"); ok {
		t.Fatal("archive promoted pending mention")
	}
	r.HandleAgentEvent("scanner", "kick-delivered", source)
	if ctx, ok := store.ActiveForAgent("scanner"); !ok || ctx.NodeID != "N" {
		t.Fatalf("mention delivery did not promote pending context: %+v ok=%v", ctx, ok)
	}
}

func TestResponderArchiveBeforeDeliveryPromotesMatchingPending(t *testing.T) {
	store := mustStore(t)
	ev := Event{Repo: "org/repo", Number: 1, NodeID: "A"}
	source := mentionKickSource(ev)
	if err := store.RecordPending("scanner", ev, source, time.Now()); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGH{app: "hive[bot]"}
	r := NewResponder(store, func() GitHub { return gh }, func() []AgentInfo {
		return []AgentInfo{{Name: "scanner", Enabled: true, Converse: true}}
	}, config.ReviewBotsConfig{MaxAttemptsPerThread: 2}, nil)
	r.HandleAgentEvent("scanner", "kick-log-archived", "archive source="+source)
	if gh.commentNumber != 1 {
		t.Fatalf("archive-before-delivery did not post matching reply, number=%d", gh.commentNumber)
	}
	if _, ok := store.ActiveForAgent("scanner"); ok {
		t.Fatal("matching context was not claimed")
	}
}

func TestResponderRepliesFIFOWhenAgentRekickedBeforeArchiveObserverRuns(t *testing.T) {
	store := mustStore(t)
	first := Event{Repo: "org/repo", Number: 1, NodeID: "A"}
	second := Event{Repo: "org/repo", Number: 2, NodeID: "B"}
	if err := store.RecordActive("scanner", first, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordActive("scanner", second, time.Now()); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGH{app: "hive[bot]"}
	r := NewResponder(store, func() GitHub { return gh }, func() []AgentInfo {
		return []AgentInfo{{Name: "scanner", Enabled: true, Converse: true}}
	}, config.ReviewBotsConfig{MaxAttemptsPerThread: 3}, nil)
	r.HandleAgentEvent("scanner", "kick-log-archived", "archive source="+mentionKickSource(first))
	if _, ok := store.ActiveForAgent("scanner"); !ok {
		t.Fatal("second active mention was cleared with the first")
	}
	if gh.commentNumber != 1 {
		t.Fatalf("reply posted to issue %d, want first queued context issue 1", gh.commentNumber)
	}
	r.HandleAgentEvent("scanner", "kick-log-archived", "archive source="+mentionKickSource(second))
	if gh.commentNumber != 2 {
		t.Fatalf("second reply posted to issue %d, want second queued context issue 2", gh.commentNumber)
	}
}

func TestResponderSilentWithoutKnownContextOrConverse(t *testing.T) {
	store := mustStore(t)
	gh := &fakeGH{app: "hive[bot]"}
	r := NewResponder(store, func() GitHub { return gh }, func() []AgentInfo {
		return []AgentInfo{{Name: "scanner", Enabled: true, Converse: false}}
	}, config.ReviewBotsConfig{MaxAttemptsPerThread: 2}, nil)
	r.HandleAgentEvent("scanner", "kick-log-archived", "archive source=mention")
	if gh.comment != "" {
		t.Fatalf("unknown context replied: %q", gh.comment)
	}
	ev := Event{Repo: "org/repo", Number: 1}
	if err := store.RecordActive("scanner", ev, time.Now()); err != nil {
		t.Fatal(err)
	}
	r.HandleAgentEvent("scanner", "kick-log-archived", "archive source="+mentionKickSource(ev))
	if gh.comment != "" {
		t.Fatalf("non-converse agent replied: %q", gh.comment)
	}
	if _, ok := store.ActiveForAgent("scanner"); ok {
		t.Fatal("non-converse active mention was not cleared")
	}
}

func TestResponderNilGitHubRequeuesAndNilAgentsDrop(t *testing.T) {
	store := mustStore(t)
	ev := Event{Repo: "org/repo", Number: 1, NodeID: "N"}
	source := mentionKickSource(ev)
	if err := store.RecordActive("scanner", ev, time.Now()); err != nil {
		t.Fatal(err)
	}
	r := NewResponder(store, func() GitHub { return nil }, func() []AgentInfo {
		return []AgentInfo{{Name: "scanner", Enabled: true, Converse: true}}
	}, config.ReviewBotsConfig{MaxAttemptsPerThread: 2}, nil)
	r.HandleAgentEvent("scanner", "kick-log-archived", "archive source="+source)
	if ctx, ok := store.ActiveForAgent("scanner"); !ok || ctx.NodeID != "N" {
		t.Fatalf("nil github did not requeue context: %+v ok=%v", ctx, ok)
	}

	r = NewResponder(store, func() GitHub { return &fakeGH{app: "hive[bot]"} }, nil, config.ReviewBotsConfig{MaxAttemptsPerThread: 2}, nil)
	r.HandleAgentEvent("scanner", "kick-log-archived", "archive source="+source)
	if _, ok := store.ActiveForAgent("scanner"); ok {
		t.Fatal("nil agent resolver did not silently drop claimed context")
	}
}

func TestResponderNilAndClaimFailureNoop(t *testing.T) {
	(*Responder)(nil).HandleAgentEvent("scanner", "kick-log-archived", "archive")
	r := NewResponder(nil, nil, nil, nil, nil)
	r.HandleAgentEvent("scanner", "kick-log-archived", "archive source=mention")
	if r.agentCanConverse("missing") {
		t.Fatal("missing agent reported converse")
	}
}

func TestResponderRateLimitAndTransientErrors(t *testing.T) {
	store := mustStore(t)
	ev := Event{Repo: "org/repo", Number: 1}
	source := mentionKickSource(ev)
	if err := store.RecordActive("scanner", ev, time.Now()); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGH{app: "hive[bot]", count: 1}
	r := NewResponder(store, func() GitHub { return gh }, func() []AgentInfo {
		return []AgentInfo{{Name: "scanner", Enabled: true, Converse: true}}
	}, config.ReviewBotsConfig{MaxAttemptsPerThread: 1}, nil)
	r.HandleAgentEvent("scanner", "kick-log-archived", "archive source="+source)
	if gh.comment != "" {
		t.Fatalf("rate-limited reply posted: %q", gh.comment)
	}
	if _, ok := store.ActiveForAgent("scanner"); ok {
		t.Fatal("rate-limited active mention was not cleared")
	}

	if err := store.RecordActive("scanner", ev, time.Now()); err != nil {
		t.Fatal(err)
	}
	gh.count = 0
	gh.countErr = errors.New("temporary count")
	r.HandleAgentEvent("scanner", "kick-log-archived", "archive source="+source)
	if _, ok := store.ActiveForAgent("scanner"); !ok {
		t.Fatal("count error cleared active mention")
	}
	gh.countErr = nil
	gh.commentErr = errors.New("temporary post")
	r.HandleAgentEvent("scanner", "kick-log-archived", "archive source="+source)
	if _, ok := store.ActiveForAgent("scanner"); !ok {
		t.Fatal("post error cleared active mention")
	}
}

func TestStoreClaimActiveIsFIFOAndRequeueRestoresHead(t *testing.T) {
	store := mustStore(t)
	if err := store.RecordActive("scanner", Event{Repo: "org/repo", Number: 1, NodeID: "A"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordActive("scanner", Event{Repo: "org/repo", Number: 2, NodeID: "B"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	first, ok, err := store.ClaimActive("scanner")
	if err != nil || !ok || first.NodeID != "A" {
		t.Fatalf("first claim = (%+v,%v,%v)", first, ok, err)
	}
	if err := store.RequeueActiveFront("scanner", first); err != nil {
		t.Fatal(err)
	}
	again, ok, err := store.ClaimActive("scanner")
	if err != nil || !ok || again.NodeID != "A" {
		t.Fatalf("requeued claim = (%+v,%v,%v)", again, ok, err)
	}
	second, ok, err := store.ClaimActive("scanner")
	if err != nil || !ok || second.NodeID != "B" {
		t.Fatalf("second claim = (%+v,%v,%v)", second, ok, err)
	}
	if _, ok, err := store.ClaimActive("scanner"); err != nil || ok {
		t.Fatalf("empty claim ok=%v err=%v", ok, err)
	}
}

func TestStoreClearActiveAndRecordEmptyAgent(t *testing.T) {
	store := mustStore(t)
	if err := store.RecordActive("", Event{Repo: "org/repo"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordActive("scanner", Event{Repo: "org/repo", Number: 1, NodeID: "A"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordActive("scanner", Event{Repo: "org/repo", Number: 2, NodeID: "B"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.ClearActive("scanner"); err != nil {
		t.Fatal(err)
	}
	if ctx, ok := store.ActiveForAgent("scanner"); !ok || ctx.NodeID != "B" {
		t.Fatalf("clear did not pop one context: %+v ok=%v", ctx, ok)
	}
	if err := store.ClearActive("scanner"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.ActiveForAgent("scanner"); ok {
		t.Fatal("second clear left active context")
	}
}

func TestStorePendingClearAndPromoteBySource(t *testing.T) {
	store := mustStore(t)
	first := Event{Repo: "org/repo", Number: 1, NodeID: "A"}
	second := Event{Repo: "org/repo", Number: 2, NodeID: "B"}
	firstSource := mentionKickSource(first)
	secondSource := mentionKickSource(second)
	if err := store.RecordPending("scanner", first, firstSource, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPending("scanner", second, secondSource, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.ClearPending("scanner", secondSource); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.PromotePending("scanner", secondSource); err != nil || ok {
		t.Fatalf("cleared pending context promoted ok=%v err=%v", ok, err)
	}
	ctx, ok, err := store.PromotePending("scanner", firstSource)
	if err != nil || !ok || ctx.NodeID != "A" {
		t.Fatalf("remaining pending context = (%+v,%v,%v)", ctx, ok, err)
	}
}

func TestMentionSourceAndObserverDetailHelpers(t *testing.T) {
	if !mentionSource(SourceMention) || !mentionSource(SourceMention+":N") {
		t.Fatal("mention source token was not recognized")
	}
	if mentionSource("governor") {
		t.Fatal("non-mention source was recognized")
	}
	detail := "kick source=" + SourceMention + ":N"
	if got := kickObserverDetailSource(detail); got != SourceMention+":N" {
		t.Fatalf("detail source = %q", got)
	}
	if got := kickObserverDetailReason(detail); got != "kick" {
		t.Fatalf("detail reason = %q", got)
	}
}

func TestCompletionReplyWithoutDetail(t *testing.T) {
	if got := completionReply("scanner", ""); !strings.Contains(got, "scanner") || strings.Contains(got, "()") {
		t.Fatalf("reply = %q", got)
	}
}
