package omp_test

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/extwork/omp"
	"github.com/hivecommons/hive/pkg/extwork/omp/fixture"
)

const (
	childEnv        = "HIVE_OMP_FIXTURE_CHILD"
	workflowDir     = "testdata/omp-fixture"
	workflowVersion = "omp-workbench/1.0.0"
	inputRevision   = "7d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60"
	identityA       = "workbench-a"
	shortTimeout    = 200 * time.Millisecond
)

// TestMain doubles as the fixture process: when the conformance tests re-exec
// this binary with childEnv set, everything after "--" is the workbench argv.
func TestMain(m *testing.M) {
	flag.Parse()
	if os.Getenv(childEnv) == "1" {
		os.Exit(fixture.Run(context.Background(), flag.Args(), os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func admissionFor(t *testing.T, workKey, task string, gen uint64, stage, mode string, hints map[string]string) (extwork.Admission, []byte) {
	t.Helper()
	adm := extwork.Admission{
		WorkKey:          workKey,
		AssignmentID:     task,
		Generation:       gen,
		Stage:            stage,
		ContractRevision: "contract-1",
		Engine:           omp.Engine,
		WorkflowVersion:  workflowVersion,
		InputRevision:    inputRevision,
		Authority:        extwork.AuthorityBinding{Identity: identityA, Tier: "T3", Capability: omp.Capability, Mode: mode},
	}
	payload, err := omp.BuildBundle(adm, "bounded summary for "+workKey, "hivecommons/omp-fixture", nil, hints)
	if err != nil {
		t.Fatal(err)
	}
	adm.RequestDigest = extwork.RequestDigest(payload)
	return adm, payload
}

// pipeLink is an in-process Link the test drives as the workbench side.
type pipeLink struct {
	toPeer   chan omp.Message // what the fake workbench sends to the peer
	fromPeer chan omp.Message // what the peer sent to the fake workbench
	closed   chan struct{}
}

func newPipeLink() *pipeLink {
	return &pipeLink{toPeer: make(chan omp.Message, 16), fromPeer: make(chan omp.Message, 16), closed: make(chan struct{})}
}

func (l *pipeLink) Send(ctx context.Context, msg omp.Message) error {
	select {
	case <-l.closed:
		return omp.ErrLinkClosed
	case l.fromPeer <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *pipeLink) Recv(ctx context.Context) (omp.Message, error) {
	select {
	case <-l.closed:
		return omp.Message{}, omp.ErrLinkClosed
	case msg := <-l.toPeer:
		return msg, nil
	case <-ctx.Done():
		return omp.Message{}, ctx.Err()
	}
}

func (l *pipeLink) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}

// workbenchSays pushes a frame from the fake workbench to the peer.
func (l *pipeLink) workbenchSays(msg omp.Message) { l.toPeer <- msg }

// answerNext replies to the NEXT frame the peer sends, and hands that frame
// back on the returned channel. Answering only after the frame is seen keeps
// the exchange ordered exactly as a real workbench would: a reply can never
// arrive before the request it answers.
func (l *pipeLink) answerNext(reply omp.Message) <-chan omp.Message {
	out := make(chan omp.Message, 1)
	go func() {
		select {
		case msg := <-l.fromPeer:
			l.toPeer <- reply
			out <- msg
		case <-time.After(2 * time.Second):
			close(out)
		}
	}()
	return out
}

// peerSent pops the next frame the peer sent, failing after a timeout.
func (l *pipeLink) peerSent(t *testing.T) omp.Message {
	t.Helper()
	select {
	case msg := <-l.fromPeer:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("peer sent nothing")
		return omp.Message{}
	}
}

func fullCaps() omp.Capabilities {
	return omp.Capabilities{RelayCapabilities: []string{"run-stage", omp.Capability}}
}

func attachedPeer(t *testing.T, b *omp.Broker, identity, incarnation, version string, caps omp.Capabilities) (*omp.Peer, *pipeLink) {
	t.Helper()
	link := newPipeLink()
	p, err := b.Attach(context.Background(), identity, incarnation, version, caps, link)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, link
}

func TestCheckReportOnly(t *testing.T) {
	base := extwork.Admission{Stage: "implement", Authority: extwork.AuthorityBinding{Mode: extwork.ModeReportOnly}}
	if err := omp.CheckReportOnly(base); err != nil {
		t.Fatalf("implement/report-only refused: %v", err)
	}
	shadow := base
	shadow.Authority.Mode = extwork.ModeShadow
	if err := omp.CheckReportOnly(shadow); err != nil {
		t.Fatalf("shadow refused: %v", err)
	}
	for stage := range omp.WriteCapableStages {
		adm := base
		adm.Stage = " " + strings.ToUpper(stage) + " "
		if err := omp.CheckReportOnly(adm); !errors.Is(err, omp.ErrWriteCapableStage) || !errors.Is(err, extwork.ErrRefused) {
			t.Errorf("stage %q = %v, want ErrWriteCapableStage", stage, err)
		}
	}
	for _, mode := range []string{extwork.ModeOff, "enforce", "publish", ""} {
		adm := base
		adm.Authority.Mode = mode
		if err := omp.CheckReportOnly(adm); !errors.Is(err, omp.ErrWriteCapableStage) {
			t.Errorf("mode %q = %v, want ErrWriteCapableStage", mode, err)
		}
	}
}

func TestCapabilitiesAndPeerGate(t *testing.T) {
	var nilCaps *omp.Capabilities
	if nilCaps.Declares(omp.Capability) {
		t.Fatal("nil capabilities declare nothing")
	}
	if !(&omp.Capabilities{RelayCapabilities: []string{" ext-exec/omp "}}).Declares(omp.Capability) {
		t.Fatal("token with whitespace not recognised")
	}
	link := newPipeLink()
	noCap := omp.Capabilities{RelayCapabilities: []string{"run-stage", "ext-exec/flue"}}
	if _, err := omp.NewPeer("wb", "inc", workflowVersion, noCap, link, nil); !errors.Is(err, omp.ErrCapabilityMissing) || !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("peer without capability = %v", err)
	}
	if _, err := omp.NewPeer("", "inc", workflowVersion, fullCaps(), link, nil); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("peer without identity = %v", err)
	}
	if _, err := omp.NewPeer("wb", "inc", workflowVersion, fullCaps(), nil, nil); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("peer without link = %v", err)
	}
	b := omp.NewBroker()
	if _, err := b.Attach(context.Background(), "wb", "inc", workflowVersion, noCap, link); !errors.Is(err, omp.ErrCapabilityMissing) {
		t.Fatalf("broker attach without capability = %v", err)
	}
	if _, ok := b.Peer("wb"); ok || len(b.Identities()) != 0 {
		t.Fatal("a refused peer was registered")
	}
	p, err := omp.NewPeer("wb", "inc", workflowVersion, fullCaps(), link, nil)
	if err != nil || p.Identity() != "wb" || p.Incarnation() != "inc" || p.Version() != workflowVersion || !p.Alive() || p.Err() != nil {
		t.Fatalf("NewPeer = %+v %v", p, err)
	}
}

func TestBrokerAttachReplaceDetach(t *testing.T) {
	b := omp.NewBroker()
	first, firstLink := attachedPeer(t, b, "wb", "inc-1", workflowVersion, fullCaps())
	second, _ := attachedPeer(t, b, "wb", "inc-2", workflowVersion, fullCaps())
	if got, _ := b.Peer("wb"); got != second {
		t.Fatal("re-attachment did not replace the peer")
	}
	select {
	case <-first.Gone():
	case <-time.After(2 * time.Second):
		t.Fatal("replaced peer's link was not closed")
	}
	if _, err := firstLink.Recv(context.Background()); !errors.Is(err, omp.ErrLinkClosed) {
		t.Fatalf("old link still open: %v", err)
	}
	// Detaching the stale peer leaves the live one in place.
	b.Detach(first)
	if got, ok := b.Peer("wb"); !ok || got != second {
		t.Fatal("detaching a stale peer removed the live one")
	}
	if ids := b.Identities(); len(ids) != 1 || ids[0] != "wb" {
		t.Fatalf("Identities = %v", ids)
	}
	_ = second.Close()
	select {
	case <-second.Gone():
	case <-time.After(2 * time.Second):
		t.Fatal("closed peer not gone")
	}
	testutil.Eventually(t, 2*time.Second, func() bool {
		_, ok := b.Peer("wb")
		return !ok
	}, "serve exit did not detach the peer")
}

func TestPeerOfferStartObserveCancelArtifact(t *testing.T) {
	ctx := context.Background()
	b := omp.NewBroker()
	p, link := attachedPeer(t, b, identityA, "inc-1", workflowVersion, fullCaps())
	adm, payload := admissionFor(t, "hivecommons/hive#1", "task-1", 2, "implement", extwork.ModeReportOnly, nil)
	key := adm.ExecutionKey()
	offer := extwork.Offer{ExecutionKey: key, Engine: omp.Engine, WorkKey: adm.WorkKey, AssignmentID: adm.AssignmentID, Stage: adm.Stage, Summary: "summary"}

	// Start before acceptance is refused Hive-side; nothing is sent.
	if _, err := p.Start(ctx, key, adm.Generation, adm.Stage, payload); !errors.Is(err, omp.ErrNotAccepted) {
		t.Fatalf("start before accept = %v", err)
	}
	if p.Accepted(key) {
		t.Fatal("key accepted before any offer")
	}
	// Offer: the frame carries the summary and identities, no payload.
	seen := link.answerNext(omp.Message{Type: omp.MsgAccept, ExecutionKey: string(key), Reason: "ok"})
	d, reason, err := p.Offer(ctx, offer, adm.Generation)
	if err != nil || d != extwork.OfferAccepted || reason != "ok" {
		t.Fatalf("Offer = %s %q %v", d, reason, err)
	}
	sent := <-seen
	if sent.Type != omp.MsgOffer || sent.Summary != "summary" || sent.TaskGen != 2 || sent.TaskID != "task-1" || len(sent.Payload) != 0 || sent.Seq == 0 {
		t.Fatalf("offer frame = %+v", sent)
	}
	if !p.Accepted(key) {
		t.Fatal("acceptance not recorded")
	}
	// Start: refused without a run id, then refused with an odd reason, then
	// a conflict, then accepted.
	link.answerNext(omp.Message{Type: omp.MsgStarted, ExecutionKey: string(key)})
	if _, err := p.Start(ctx, key, adm.Generation, adm.Stage, payload); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("started without run id = %v", err)
	}
	link.answerNext(omp.Message{Type: omp.MsgStartRefused, ExecutionKey: string(key), Reason: "busy"})
	if _, err := p.Start(ctx, key, adm.Generation, adm.Stage, payload); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("refused start = %v", err)
	}
	link.answerNext(omp.Message{Type: omp.MsgStartRefused, ExecutionKey: string(key), Reason: omp.ReasonConflict})
	if _, err := p.Start(ctx, key, adm.Generation, adm.Stage, payload); !errors.Is(err, extwork.ErrConflict) {
		t.Fatalf("workbench conflict = %v", err)
	}
	if _, err := p.Start(ctx, key, adm.Generation, adm.Stage, []byte("other")); !errors.Is(err, extwork.ErrConflict) {
		t.Fatalf("local conflict = %v", err)
	}
	seen = link.answerNext(omp.Message{Type: omp.MsgStarted, ExecutionKey: string(key), RemoteRunID: "run-1"})
	res, err := p.Start(ctx, key, adm.Generation, adm.Stage, payload)
	if err != nil || res.RemoteRunID != "run-1" || res.RemoteIncarnation != "inc-1" || res.Deduplicated {
		t.Fatalf("Start = %+v %v", res, err)
	}
	startFrame := <-seen
	if startFrame.Type != omp.MsgStart || string(startFrame.Payload) != string(payload) || startFrame.TaskGen != 2 {
		t.Fatalf("start frame = %+v", startFrame)
	}
	obs, err := p.Observe(key)
	if err != nil || obs.State != extwork.StateAccepted || obs.RemoteRunID != "run-1" || obs.Stage != adm.Stage {
		t.Fatalf("Observe after start = %+v %v (stage must be seeded from the offer)", obs, err)
	}
	// A progress frame that omits the stage keeps the one on record.
	link.workbenchSays(omp.Message{Type: omp.MsgProgress, ExecutionKey: string(key), State: "waiting"})
	waitState(t, p, key, extwork.StateWaiting)
	if obs, _ := p.Observe(key); obs.Stage != adm.Stage {
		t.Fatalf("stage lost on a frame without one: %+v", obs)
	}
	// Progress: valid, invalid, and a frame for a key never started.
	link.workbenchSays(omp.Message{Type: omp.MsgProgress, ExecutionKey: string(key), State: "running", Stage: "review"})
	link.workbenchSays(omp.Message{Type: omp.MsgProgress, ExecutionKey: string(key), State: "odd", Stage: "review"})
	link.workbenchSays(omp.Message{Type: omp.MsgProgress, ExecutionKey: "stranger", State: "running"})
	link.workbenchSays(omp.Message{Type: omp.MsgProgress}) // no key: ignored
	waitState(t, p, key, extwork.StateUnknown)
	obs, _ = p.Observe(key)
	if !strings.Contains(obs.Detail, "odd") || obs.Stage != "review" {
		t.Fatalf("invalid state observation = %+v", obs)
	}
	if _, err := p.Observe("stranger"); !errors.Is(err, extwork.ErrNotFound) {
		t.Fatalf("stranger = %v", err)
	}
	// Cancel: acknowledged but not stopped.
	link.answerNext(omp.Message{Type: omp.MsgCancelAck, ExecutionKey: string(key), Acknowledged: true, Detail: "ignored"})
	facts, err := p.Cancel(ctx, key)
	if err != nil || !facts.Requested || !facts.Acknowledged || facts.Stopped || facts.Detail != "ignored" {
		t.Fatalf("Cancel = %+v %v", facts, err)
	}
	if _, err := p.Cancel(ctx, "stranger"); !errors.Is(err, extwork.ErrNotFound) {
		t.Fatalf("cancel stranger = %v", err)
	}
	// Receipt: unsafe path refused, nil artifact ignored, good one stored.
	link.workbenchSays(omp.Message{Type: omp.MsgReceipt, ExecutionKey: string(key), Artifact: &omp.Artifact{Path: "../x", Body: []byte("x")}})
	link.workbenchSays(omp.Message{Type: omp.MsgReceipt, ExecutionKey: string(key)})
	link.workbenchSays(omp.Message{Type: omp.MsgReceipt, ExecutionKey: string(key), RemoteRunID: "run-1", Stage: "report", Artifact: &omp.Artifact{Path: omp.ReceiptArtifact, Digest: "d", Size: 3, Body: []byte("abc")}})
	waitState(t, p, key, extwork.StateTerminal)
	obs, _ = p.Observe(key)
	if obs.Receipt == nil || obs.Receipt.Path != omp.ReceiptArtifact || obs.Stage != "report" {
		t.Fatalf("terminal observation = %+v", obs)
	}
	// A progress frame after terminal changes nothing. Frames are handled
	// in order, so once the marker frame that follows it is visible the
	// ignored one has been processed.
	link.workbenchSays(omp.Message{Type: omp.MsgProgress, ExecutionKey: string(key), State: "running"})
	link.workbenchSays(omp.Message{Type: omp.MsgAccept, ExecutionKey: "unrequested"}) // parks a decision nobody waits for
	link.workbenchSays(omp.Message{Type: omp.MsgProgress, ExecutionKey: "marker", RemoteRunID: "m", State: "running"})
	waitState(t, p, "marker", extwork.StateRunning)
	if obs, _ := p.Observe(key); obs.State != extwork.StateTerminal {
		t.Fatalf("terminal run moved: %+v", obs)
	}
	rc, err := p.OpenArtifact(key, omp.ReceiptArtifact)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(raw) != "abc" {
		t.Fatalf("artifact = %q", raw)
	}
	if _, err := p.OpenArtifact(key, "../x"); !errors.Is(err, extwork.ErrReceiptPath) {
		t.Fatalf("unsafe artifact path = %v", err)
	}
	if _, err := p.OpenArtifact(key, "missing"); !errors.Is(err, extwork.ErrNotFound) {
		t.Fatalf("missing artifact = %v", err)
	}
	if _, err := p.OpenArtifact("stranger", omp.ReceiptArtifact); !errors.Is(err, extwork.ErrNotFound) {
		t.Fatalf("stranger artifact = %v", err)
	}
	// A terminal run survives the link going away; a live one goes unknown.
	live, livePayload := admissionFor(t, "hivecommons/hive#2", "task-2", 1, "implement", extwork.ModeReportOnly, nil)
	link.answerNext(omp.Message{Type: omp.MsgAccept, ExecutionKey: string(live.ExecutionKey())})
	if d, _, _ := p.Offer(ctx, extwork.Offer{ExecutionKey: live.ExecutionKey()}, 1); d != extwork.OfferAccepted {
		t.Fatal("live offer not accepted")
	}
	link.answerNext(omp.Message{Type: omp.MsgStarted, ExecutionKey: string(live.ExecutionKey()), RemoteRunID: "run-2"})
	if _, err := p.Start(ctx, live.ExecutionKey(), 1, live.Stage, livePayload); err != nil {
		t.Fatal(err)
	}
	_ = link.Close()
	<-p.Gone()
	if obs, err := p.Observe(key); err != nil || obs.State != extwork.StateTerminal {
		t.Fatalf("terminal after gone = %+v %v", obs, err)
	}
	obs, err = p.Observe(live.ExecutionKey())
	if !errors.Is(err, omp.ErrPeerGone) || !errors.Is(err, extwork.ErrTransport) || obs.State != extwork.StateUnknown {
		t.Fatalf("live after gone = %+v %v", obs, err)
	}
	// The read-loop error that ended the link is the reason the state is
	// unknown, and Observe carries it.
	if loopErr := p.Err(); !errors.Is(loopErr, omp.ErrLinkClosed) || !strings.Contains(err.Error(), loopErr.Error()) {
		t.Fatalf("read-loop error %v not surfaced by Observe: %v", loopErr, err)
	}
	if _, err := p.Start(ctx, live.ExecutionKey(), 1, live.Stage, livePayload); !errors.Is(err, omp.ErrPeerGone) {
		t.Fatalf("start after gone = %v", err)
	}
	if facts, err := p.Cancel(ctx, live.ExecutionKey()); !errors.Is(err, omp.ErrPeerGone) || facts.Stopped {
		t.Fatalf("cancel after gone = %+v %v", facts, err)
	}
	if d, _, err := p.Offer(ctx, offer, 1); d != extwork.OfferDeclined || !errors.Is(err, omp.ErrPeerGone) {
		t.Fatalf("offer after gone = %s %v", d, err)
	}
}

func waitState(t *testing.T, p *omp.Peer, key extwork.ExecutionKey, want extwork.State) {
	t.Helper()
	testutil.Eventually(t, 2*time.Second, func() bool {
		obs, _ := p.Observe(key)
		return obs.State == want
	}, "state %s not reached for %s", want, key)
}

func TestPeerWaitsAreBounded(t *testing.T) {
	b := omp.NewBroker()
	p, link := attachedPeer(t, b, identityA, "inc-1", workflowVersion, fullCaps())
	adm, payload := admissionFor(t, "hivecommons/hive#3", "task-3", 1, "implement", extwork.ModeReportOnly, nil)
	key := adm.ExecutionKey()
	ctx, cancel := context.WithTimeout(context.Background(), shortTimeout)
	defer cancel()
	// An unanswered offer is declined when the context ends, never accepted.
	if d, _, err := p.Offer(ctx, extwork.Offer{ExecutionKey: key}, 1); d != extwork.OfferDeclined || !errors.Is(err, omp.ErrOfferPending) {
		t.Fatalf("unanswered offer = %s %v", d, err)
	}
	link.peerSent(t)
	// A late acceptance (the workbench answers the withdrawn offer after the
	// fact) is kept for the key, so a re-offer resolves at once; an
	// unacknowledged start and cancel end with the context.
	link.workbenchSays(omp.Message{Type: omp.MsgAccept, ExecutionKey: string(key)})
	if d, _, err := p.Offer(context.Background(), extwork.Offer{ExecutionKey: key}, 1); d != extwork.OfferAccepted || err != nil {
		t.Fatalf("second offer = %s %v", d, err)
	}
	link.peerSent(t)
	ctx2, cancel2 := context.WithTimeout(context.Background(), shortTimeout)
	defer cancel2()
	if _, err := p.Start(ctx2, key, 1, adm.Stage, payload); !errors.Is(err, extwork.ErrTransport) {
		t.Fatalf("unacknowledged start = %v", err)
	}
	link.peerSent(t)
	link.answerNext(omp.Message{Type: omp.MsgStarted, ExecutionKey: string(key), RemoteRunID: "r"})
	if _, err := p.Start(context.Background(), key, 1, adm.Stage, payload); err != nil {
		t.Fatal(err)
	}
	ctx3, cancel3 := context.WithTimeout(context.Background(), shortTimeout)
	defer cancel3()
	if facts, err := p.Cancel(ctx3, key); !errors.Is(err, extwork.ErrTransport) || facts.Stopped || !facts.Requested {
		t.Fatalf("unacknowledged cancel = %+v %v", facts, err)
	}
}

func TestAdapterConstructionAndGates(t *testing.T) {
	if _, err := omp.New(omp.Config{}); err == nil {
		t.Fatal("empty version accepted")
	}
	if _, err := omp.Factory(map[string]string{}); err == nil {
		t.Fatal("Factory without settings succeeded")
	}
	a, err := omp.Factory(map[string]string{omp.SettingWorkflowVersion: workflowVersion})
	if err != nil || a.Engine() != omp.Engine {
		t.Fatalf("Factory = %v %v", a, err)
	}
	if _, err := omp.BuildBundle(extwork.Admission{}, "s", "r", map[string]string{"../x": "y"}, nil); !errors.Is(err, extwork.ErrReceiptPath) {
		t.Fatalf("bundle with escaping file path = %v", err)
	}

	ctx := context.Background()
	b := omp.NewBroker()
	adapter, err := omp.New(omp.Config{Peers: b, WorkflowVersion: workflowVersion, OfferTimeout: shortTimeout, AckTimeout: shortTimeout})
	if err != nil || adapter.WorkflowVersion() != workflowVersion {
		t.Fatal(err)
	}
	adm, payload := admissionFor(t, "hivecommons/hive#4", "task-4", 1, "implement", extwork.ModeReportOnly, nil)
	req := extwork.StartRequest{Admission: adm, Payload: payload}

	// No workbench attached: the host declines and Start is a transport
	// failure, never a start.
	if d, _, err := adapter.Host(identityA, adm).Decide(ctx, extwork.Offer{ExecutionKey: adm.ExecutionKey()}); d != extwork.OfferDeclined || !errors.Is(err, extwork.ErrTransport) {
		t.Fatalf("no peer host = %s %v", d, err)
	}
	if _, err := adapter.Start(ctx, req); !errors.Is(err, extwork.ErrTransport) {
		t.Fatalf("no peer start = %v", err)
	}
	if _, err := adapter.Observe(ctx, adm.ExecutionKey(), ""); !errors.Is(err, extwork.ErrNotFound) {
		t.Fatalf("never started observe = %v", err)
	}
	if _, err := adapter.Cancel(ctx, adm.ExecutionKey(), ""); !errors.Is(err, extwork.ErrNotFound) {
		t.Fatalf("never started cancel = %v", err)
	}
	if _, err := adapter.OpenArtifact(ctx, adm.ExecutionKey(), "", omp.ReceiptArtifact); !errors.Is(err, extwork.ErrNotFound) {
		t.Fatalf("never started artifact = %v", err)
	}
	if _, err := adapter.OpenArtifact(ctx, adm.ExecutionKey(), "", "/abs"); !errors.Is(err, extwork.ErrReceiptPath) {
		t.Fatalf("absolute artifact path = %v", err)
	}

	// Admission gates, checked before any peer is consulted.
	invalid := adm
	invalid.WorkKey = ""
	if _, err := adapter.Start(ctx, extwork.StartRequest{Admission: invalid, Payload: payload}); !errors.Is(err, extwork.ErrInvalidAdmission) {
		t.Errorf("invalid admission = %v", err)
	}
	noCap := adm
	noCap.Authority.Capability = "run-stage"
	if _, err := adapter.Start(ctx, extwork.StartRequest{Admission: noCap, Payload: payload}); !errors.Is(err, extwork.ErrRefused) {
		t.Errorf("missing capability start = %v", err)
	}
	if d, _, err := adapter.Host(identityA, noCap).Decide(ctx, extwork.Offer{}); d != extwork.OfferDeclined || !errors.Is(err, omp.ErrCapabilityMissing) {
		t.Errorf("missing capability host = %s %v", d, err)
	}
	wrongEngine := adm
	wrongEngine.Engine = "flue"
	if _, err := adapter.Start(ctx, extwork.StartRequest{Admission: wrongEngine, Payload: payload}); !errors.Is(err, extwork.ErrRefused) {
		t.Errorf("wrong engine = %v", err)
	}
	wrongVersion := adm
	wrongVersion.WorkflowVersion = "omp-workbench/9"
	if _, err := adapter.Start(ctx, extwork.StartRequest{Admission: wrongVersion, Payload: payload}); !errors.Is(err, extwork.ErrRefused) {
		t.Errorf("wrong version = %v", err)
	}
	write := adm
	write.Stage = "publish"
	if _, err := adapter.Start(ctx, extwork.StartRequest{Admission: write, Payload: payload}); !errors.Is(err, omp.ErrWriteCapableStage) {
		t.Errorf("write-capable stage start = %v", err)
	}
	if d, _, err := adapter.Host(identityA, write).Decide(ctx, extwork.Offer{}); d != extwork.OfferDeclined || !errors.Is(err, omp.ErrWriteCapableStage) {
		t.Errorf("write-capable stage host = %s %v", d, err)
	}
	if _, err := adapter.Start(ctx, extwork.StartRequest{Admission: adm, Payload: []byte("other")}); !errors.Is(err, extwork.ErrPayloadDigest) {
		t.Errorf("payload digest = %v", err)
	}

	// A peer on the wrong version is refused, never downgraded.
	_, _ = attachedPeer(t, b, identityA, "inc-old", "omp-workbench/0.9.0", fullCaps())
	if _, err := adapter.Start(ctx, req); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("stale peer version = %v", err)
	}
	if d, _, err := adapter.Host(identityA, adm).Decide(ctx, extwork.Offer{}); d != extwork.OfferDeclined || !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("stale peer host = %s %v", d, err)
	}

	// The right peer: offer, then a pinned incarnation that no longer
	// matches is a mismatch at Start and at Observe.
	p, link := attachedPeer(t, b, identityA, "inc-new", workflowVersion, fullCaps())
	link.answerNext(omp.Message{Type: omp.MsgAccept, ExecutionKey: string(adm.ExecutionKey())})
	if d, _, err := adapter.Host(identityA, adm).Decide(ctx, extwork.Offer{ExecutionKey: adm.ExecutionKey()}); d != extwork.OfferAccepted || err != nil {
		t.Fatalf("host accept = %s %v", d, err)
	}
	pinnedElsewhere := adm
	pinnedElsewhere.EngineIncarnation = "inc-gone"
	if _, err := adapter.Start(ctx, extwork.StartRequest{Admission: pinnedElsewhere, Payload: payload}); !errors.Is(err, extwork.ErrIncarnationMismatch) {
		t.Fatalf("pinned to another incarnation = %v", err)
	}
	link.answerNext(omp.Message{Type: omp.MsgStarted, ExecutionKey: string(adm.ExecutionKey()), RemoteRunID: "run-4"})
	res, err := adapter.Start(ctx, req)
	if err != nil || res.RemoteRunID != "run-4" || res.RemoteIncarnation != "inc-new" {
		t.Fatalf("Start = %+v %v", res, err)
	}
	if obs, err := adapter.Observe(ctx, adm.ExecutionKey(), ""); err != nil || obs.State != extwork.StateAccepted {
		t.Fatalf("Observe = %+v %v", obs, err)
	}
	if _, err := adapter.Observe(ctx, adm.ExecutionKey(), "inc-old"); !errors.Is(err, extwork.ErrIncarnationMismatch) {
		t.Fatalf("observe with stale pin = %v", err)
	}
	// Shadow mode never reaches the workbench: the host answers locally.
	shadow := adm
	shadow.Authority.Mode = extwork.ModeShadow
	if d, reason, err := adapter.Host(identityA, shadow).Decide(ctx, extwork.Offer{ExecutionKey: "shadow-key"}); d != extwork.OfferAccepted || err != nil || !strings.Contains(reason, "shadow") {
		t.Fatalf("shadow host = %s %q %v", d, reason, err)
	}
	select {
	case msg := <-link.fromPeer:
		t.Fatalf("shadow offer reached the workbench: %+v", msg)
	case <-time.After(shortTimeout):
	}
	// The workbench reconnects under a new incarnation: the run this
	// adapter bound is never adopted from the new instance.
	_, _ = attachedPeer(t, b, identityA, "inc-recreated", workflowVersion, fullCaps())
	<-p.Gone()
	if _, err := adapter.Observe(ctx, adm.ExecutionKey(), ""); !errors.Is(err, extwork.ErrIncarnationMismatch) {
		t.Fatalf("observe after recreate = %v", err)
	}
	if facts, err := adapter.Cancel(ctx, adm.ExecutionKey(), ""); !errors.Is(err, extwork.ErrIncarnationMismatch) || facts.Stopped {
		t.Fatalf("cancel after recreate = %+v %v", facts, err)
	}
	if _, err := adapter.OpenArtifact(ctx, adm.ExecutionKey(), "", omp.ReceiptArtifact); !errors.Is(err, extwork.ErrIncarnationMismatch) {
		t.Fatalf("artifact after recreate = %v", err)
	}
	// And once no peer holds the identity at all, the run is simply gone.
	b.Detach(mustPeer(t, b, identityA))
	if _, err := adapter.Observe(ctx, adm.ExecutionKey(), ""); !errors.Is(err, omp.ErrPeerGone) {
		t.Fatalf("observe with no peer = %v", err)
	}
}

func mustPeer(t *testing.T, b *omp.Broker, identity string) *omp.Peer {
	t.Helper()
	p, ok := b.Peer(identity)
	if !ok {
		t.Fatalf("no peer for %s", identity)
	}
	return p
}

func TestWSLinkAndListener(t *testing.T) {
	if _, err := omp.Listen("0.0.0.0:0", omp.NewBroker()); !errors.Is(err, omp.ErrNotLoopback) {
		t.Fatalf("non-loopback listen = %v", err)
	}
	if _, err := omp.Listen("not-an-address", omp.NewBroker()); err == nil {
		t.Fatal("bad address accepted")
	}
	b := omp.NewBroker()
	l, err := omp.Listen("", b)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if !strings.HasPrefix(l.URL(), "ws://127.0.0.1:") {
		t.Fatalf("URL = %s", l.URL())
	}
	// A workbench that lacks the capability is refused at hello.
	if _, err := fixture.Start(fixture.Options{HubURL: l.URL(), Identity: "no-cap", WorkflowDir: workflowDir, Capabilities: []string{"run-stage"}}); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("workbench without capability = %v", err)
	}
	if _, ok := b.Peer("no-cap"); ok {
		t.Fatal("refused workbench was attached")
	}
	// A workbench with it attaches, and the link round-trips frames.
	w, err := fixture.Start(fixture.Options{HubURL: l.URL(), Identity: "cap", WorkflowDir: workflowDir, Incarnation: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	p := mustPeer(t, b, "cap")
	if p.Incarnation() != "s1" || p.Version() != workflowVersion {
		t.Fatalf("attached peer = %s %s", p.Incarnation(), p.Version())
	}
	// Closing the listener drops the link; the peer is gone and its link
	// refuses further sends.
	_ = l.Close()
	select {
	case <-p.Gone():
	case <-time.After(2 * time.Second):
		t.Fatal("listener close did not drop the peer")
	}
	if _, _, err := p.Offer(context.Background(), extwork.Offer{ExecutionKey: "k"}, 1); !errors.Is(err, omp.ErrPeerGone) {
		t.Fatalf("offer on a closed link = %v", err)
	}
}
