//go:build integration && extwork_omp

package extworksmoke

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/extwork/omp"
)

const ompSmokeVersion = "omp-smoke/1.0.0"

type ompSmokeLink struct {
	toPeer   chan omp.Message
	fromPeer chan omp.Message
	closed   chan struct{}
}

func newOMPSmokeLink() *ompSmokeLink {
	return &ompSmokeLink{toPeer: make(chan omp.Message, 8), fromPeer: make(chan omp.Message, 8), closed: make(chan struct{})}
}

func (l *ompSmokeLink) Send(ctx context.Context, msg omp.Message) error {
	select {
	case <-l.closed:
		return omp.ErrLinkClosed
	case l.fromPeer <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *ompSmokeLink) Recv(ctx context.Context) (omp.Message, error) {
	select {
	case <-l.closed:
		return omp.Message{}, omp.ErrLinkClosed
	case msg := <-l.toPeer:
		return msg, nil
	case <-ctx.Done():
		return omp.Message{}, ctx.Err()
	}
}

func (l *ompSmokeLink) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}

func (l *ompSmokeLink) nextFromPeer(t *testing.T) omp.Message {
	t.Helper()
	select {
	case msg := <-l.fromPeer:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("peer sent no OMP frame")
		return omp.Message{}
	}
}

func TestOMPSmokeAdapterRegistersHandshakeAndDispatches(t *testing.T) {
	broker := omp.NewBroker()
	link := newOMPSmokeLink()
	peer, err := broker.Attach(context.Background(), "workbench-smoke", "omp-smoke-incarnation", ompSmokeVersion, omp.Capabilities{RelayCapabilities: []string{"run-stage", omp.Capability}}, link)
	if err != nil {
		t.Fatalf("attach OMP peer: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })

	reg := extwork.NewRegistry()
	reg.Register(omp.Engine, func(settings map[string]string) (extwork.Adapter, error) {
		return omp.New(omp.Config{Peers: broker, WorkflowVersion: settings[omp.SettingWorkflowVersion], OfferTimeout: 2 * time.Second, AckTimeout: 2 * time.Second})
	})
	adapter, err := reg.Open(omp.Engine, map[string]string{omp.SettingWorkflowVersion: ompSmokeVersion})
	if err != nil {
		t.Fatalf("open registered omp adapter: %v", err)
	}

	payload := []byte(`{"summary":"report-only OMP smoke task"}`)
	adm := extwork.Admission{
		WorkKey: "hivecommons/hive#6899", AssignmentID: "omp-smoke-task", Generation: 1, Stage: "implement",
		ContractRevision: "smoke/v1", Engine: omp.Engine, WorkflowVersion: ompSmokeVersion,
		InputRevision: "0123456789abcdef0123456789abcdef01234567",
		Authority:     extwork.AuthorityBinding{Identity: "workbench-smoke", Tier: "T3", Capability: omp.Capability, Mode: extwork.ModeReportOnly},
	}
	adm.RequestDigest = extwork.RequestDigest(payload)

	binding := extwork.New(adapter, extwork.NewMemoryStore(), nil, nil, extwork.ModeReportOnly)
	offerDone := make(chan error, 1)
	go func() {
		_, err := binding.Offer(context.Background(), adapter.(interface {
			Host(string, extwork.Admission) extwork.Host
		}).Host("workbench-smoke", adm), adm, "bounded summary before bundle")
		offerDone <- err
	}()
	offer := link.nextFromPeer(t)
	if offer.Type != omp.MsgOffer || offer.Summary == "" || len(offer.Payload) != 0 {
		t.Fatalf("offer frame = %+v", offer)
	}
	link.toPeer <- omp.Message{Type: omp.MsgAccept, ExecutionKey: offer.ExecutionKey, Reason: "auth_ok"}
	if err := <-offerDone; err != nil {
		t.Fatalf("offer: %v", err)
	}

	startDone := make(chan extwork.DispatchResult, 1)
	errDone := make(chan error, 1)
	go func() {
		res, err := binding.Dispatch(context.Background(), adm, payload)
		startDone <- res
		errDone <- err
	}()
	start := link.nextFromPeer(t)
	if start.Type != omp.MsgStart || len(start.Payload) == 0 || start.Summary != "" {
		t.Fatalf("start frame = %+v", start)
	}
	link.toPeer <- omp.Message{Type: omp.MsgStarted, ExecutionKey: start.ExecutionKey, RemoteRunID: "omp-task-1"}
	res := <-startDone
	if err := <-errDone; err != nil {
		t.Fatalf("dispatch OMP smoke task: %v", err)
	}
	if !res.Started || res.Run.RemoteRunID != "omp-task-1" || res.Run.RemoteIncarnation != "omp-smoke-incarnation" {
		t.Fatalf("dispatch result = %+v", res)
	}

	writeCapable := adm
	writeCapable.Stage = "publish"
	if _, err := binding.Dispatch(context.Background(), writeCapable, payload); !errors.Is(err, omp.ErrWriteCapableStage) {
		t.Fatalf("write-capable stage error = %v, want ErrWriteCapableStage", err)
	}
	select {
	case msg := <-link.fromPeer:
		t.Fatalf("write-capable stage sent frame before refusal: %+v", msg)
	default:
	}

	if err := link.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-peer.Gone():
	case <-time.After(2 * time.Second):
		t.Fatal("peer did not observe disconnect")
	}
	obs, err := adapter.Observe(context.Background(), adm.ExecutionKey(), "omp-smoke-incarnation")
	if !errors.Is(err, extwork.ErrTransport) || obs.State != extwork.StateUnknown {
		t.Fatalf("observe after disconnect = %+v, %v", obs, err)
	}
}
